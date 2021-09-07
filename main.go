package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"
	//"reflect"
	"github.com/florianl/go-tc"
	"github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
	"github.com/influxdata/influxdb-client-go/v2/domain"
	"github.com/jsimonetti/rtnetlink"
)

var influxdb_url = flag.String("db", "http://127.0.0.1:8086", "influxdb的地址")
var org = flag.String("org", "ServerMonitor", "influxdb的org")
var bucket = flag.String("bucket", "", "influxdb的bucket，如果不指定，则使用主机名")
var token = flag.String("token", "ThisIsATokenExample=", "influxdb的token")
var scrape_netdev = flag.Bool("netdev", true, "采集netdev数据")
var scrape_qdisc = flag.Bool("qdisc", true, "采集qdisc数据")
var scrape_interval = flag.Uint("interval", 0, "采集间隔，单位ms，为0时表示只采集一次")
var print_stat = flag.Bool("print", true, "输出采集的数据")

var writeAPI api.WriteAPI
var rtnl_conn *rtnetlink.Conn
var tc_conn *tc.Tc
var c chan os.Signal
var stop_scrape bool

func getNetDev() {
	// 模仿 /proc/net/dev 输出，一模一样
	record_time := time.Now()
	// Request a list of interfaces
	msg, err := rtnl_conn.Link.List()
	if err != nil {
		log.Fatal(err)
	}
	if *print_stat {
		fmt.Println("Inter-|   Receive                                                |  Transmit")
		fmt.Println(" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed")
	}
	for _, value := range msg {
		attr := value.Attributes
		stat := attr.Stats64
		if *print_stat {
			//print stats
			fmt.Printf("%6s: %7d %7d %4d %4d %4d %5d %10d %9d %8d %7d %4d %4d %4d %5d %7d %9d\n", attr.Name,
				stat.RXBytes, stat.RXPackets, stat.RXErrors, stat.RXDropped, stat.RXFIFOErrors, stat.RXFrameErrors, stat.RXCompressed, stat.Multicast,
				stat.TXBytes, stat.TXPackets, stat.TXErrors, stat.TXDropped, stat.TXFIFOErrors, stat.Collisions, stat.TXCarrierErrors, stat.TXCompressed)
		}
		// Create point using full params constructor
		p := influxdb2.NewPoint("netdev",
			map[string]string{"interface": attr.Name},
			map[string]interface{}{
				"RXBytes": stat.RXBytes, "RXPackets": stat.RXPackets, "RXErrors": stat.RXErrors, "RXDropped": stat.RXDropped,
				"TXBytes": stat.TXBytes, "TXPackets": stat.TXPackets, "TXErrors": stat.TXErrors, "TXDropped": stat.TXDropped,
			},
			record_time)
		// write point asynchronously
		writeAPI.WritePoint(p)
	}
	// Flush writes
	writeAPI.Flush()
}

func getQdiscStat() {
	//读取tc统计数据
	record_time := time.Now()
	qdiscs, err := tc_conn.Qdisc().Get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not get qdiscs: %v\n", err)
		return
	}
	for _, qdisc := range qdiscs {
		iface, err := net.InterfaceByIndex(int(qdisc.Ifindex))
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not get interface from id %d: %v", qdisc.Ifindex, err)
			return
		}
		if *print_stat {
			fmt.Printf("%s %s parent:%d\n", iface.Name, qdisc.Kind, qdisc.Parent)
			fmt.Printf("Bytes: %d Packets: %d Drops: %d Overlimits: %d\n", qdisc.Stats.Bytes, qdisc.Stats.Packets, qdisc.Stats.Drops, qdisc.Stats.Overlimits)
			fmt.Printf("Bps: %d Pps: %d Qlen: %d Backlog: %d\n", qdisc.Stats.Bps, qdisc.Stats.Pps, qdisc.Stats.Qlen, qdisc.Stats.Backlog)
		}
		// Create point using full params constructor
		p := influxdb2.NewPoint("qdisc",
			map[string]string{"interface": iface.Name, "kind": qdisc.Kind, "parent": strconv.Itoa(int(qdisc.Parent))},
			map[string]interface{}{
				"Bytes": qdisc.Stats.Bytes, "Packets": qdisc.Stats.Packets,
				"Drops": qdisc.Stats.Drops, "Overlimits": qdisc.Stats.Overlimits, "Backlog": qdisc.Stats.Backlog,
			},
			record_time)
		// write point asynchronously
		writeAPI.WritePoint(p)
	}
	// Flush writes
	writeAPI.Flush()
}

func do_scrape() {
	if *scrape_netdev {
		getNetDev()
	}

	if *scrape_qdisc {
		getQdiscStat()
	}
}

func main() {
	//解析参数
	flag.Parse()

	//设置信号捕获
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)
	go func() {
		<-c //等待信号
		stop_scrape = true
	}()

	//获取主机名
	hostname, err := os.Hostname()
	if err != nil {
		log.Fatal(err)
	}
	if *bucket == "" {
		//使用hostname作为bucket
		bucket = &hostname
	}

	//连接influxdb
	client := influxdb2.NewClient(*influxdb_url, *token)
	defer client.Close()
	writeAPI = client.WriteAPI(*org, *bucket)
	//fmt.Println(reflect.TypeOf(writeAPI))

	// Get organization that will own new bucket
	ctx := context.Background()
	_org, err := client.OrganizationsAPI().FindOrganizationByName(ctx, *org)
	if err != nil {
		log.Fatal(err)
	}
	//如果bucket不存在，则创建bucket
	bucketsAPI := client.BucketsAPI()
	_, err = bucketsAPI.FindBucketByName(ctx, *bucket)
	if err != nil {
		_, err = bucketsAPI.CreateBucketWithName(ctx, _org, *bucket, domain.RetentionRule{EverySeconds: 3600 * 24 * 7})
		if err != nil {
			log.Fatal(err)
		}
	}

	if *scrape_netdev {
		// Dial a connection to the rtnetlink socket
		rtnl_conn, err = rtnetlink.Dial(nil)
		if err != nil {
			log.Fatal(err)
		}
		defer rtnl_conn.Close()
	}

	if *scrape_qdisc {
		// open a rtnetlink socket
		tc_conn, err = tc.Open(&tc.Config{})
		if err != nil {
			log.Fatal(err)
		}
		defer tc_conn.Close()
	}

	if *scrape_interval == 0 {
		do_scrape()
	} else {
		for {
			if !stop_scrape {
				do_scrape()
				time.Sleep(time.Duration(*scrape_interval) * time.Millisecond)
			} else {
				break
			}
		}
	}

}
