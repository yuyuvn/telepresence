package main

import (
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/spf13/pflag"
)

const (
	TOTAL_THREADS       = 3
	REQUESTS_PER_THREAD = 5
	TIMEOUT_S           = 8
	POOL_SIZE           = 3
)

func usage() {
	fmt.Printf("Usage: %s [--lock] SERVER\n", os.Args[0])
	os.Exit(1)
}

func main() {
	withLock := pflag.Bool("lock", false, "Whether to use a lock around DNS requests")
	withConnectionPool := pflag.Bool("connection-pool", false, "Whether to use a connection pool")
	pflag.Parse()
	addr := pflag.Arg(0)
	if addr == "" {
		usage()
	}

	lock := sync.Mutex{}
	connPool := NewConnectionPool(addr, POOL_SIZE)
	conn, err := dns.Dial("udp", net.JoinHostPort(addr, "53"))
	if err != nil {
		fmt.Printf("failed to create conn: %v", err)
		os.Exit(1)
	}
	dc := &dns.Client{
		Net:            "udp",
		Timeout:        TIMEOUT_S * time.Second,
		SingleInflight: true,
	}

	errors := make(chan error)
	wg := &sync.WaitGroup{}
	wg.Add(TOTAL_THREADS)
	fmt.Printf("Starting test with server %s, %d threads and %d requests per thread. Locking is %t\n", addr, TOTAL_THREADS, REQUESTS_PER_THREAD, *withLock)
	for i := 0; i < TOTAL_THREADS; i++ {
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < REQUESTS_PER_THREAD; j++ {
				msg := new(dns.Msg)
				domain := fmt.Sprintf("dns-test-%d.preview.edgestack.me.", idx)
				msg.SetQuestion(domain, dns.TypeMX)
				var err error

				if *withConnectionPool {
					_, _, err = connPool.Exchange(dc, msg)
				} else {
					if *withLock {
						lock.Lock()
					}
					_, _, err = dc.ExchangeWithConn(msg, conn)
					if *withLock {
						lock.Unlock()
					}

				}
				errors <- err
			}
		}(i)
	}
	go func() {
		wg.Wait()
		close(errors)
	}()
	totalFailed := 0
	timeouts := 0
	temporary := 0
	for err := range errors {
		if err != nil {
			totalFailed++
			fmt.Print("x")
			if err, ok := err.(net.Error); ok {
				switch {
				case err.Timeout():
					timeouts++
				case err.Temporary():
					temporary++
				default:
				}
			}

		} else {
			fmt.Print(".")
		}
	}
	fmt.Println()
	fmt.Printf("%d/%d DNS requests failed\n", totalFailed, TOTAL_THREADS*REQUESTS_PER_THREAD)
	fmt.Printf("Of which: %d/%d timeouts, %d/%d temporary, %d/%d other\n", timeouts, totalFailed, temporary, totalFailed, totalFailed-(timeouts+temporary), totalFailed)
}

type ConnectionPool struct {
	mutex sync.Mutex
	items map[*dns.Conn]bool
}

func NewConnectionPool(addr string, poolSize int) *ConnectionPool {
	connectionPool := &ConnectionPool{
		items: make(map[*dns.Conn]bool, poolSize),
	}
	for i := 0; i < poolSize; i++ {
		conn, err := dns.Dial("udp", net.JoinHostPort(addr, "53"))
		if err != nil {
			fmt.Printf("failed to create conn: %v", err)
			os.Exit(1)
		}
		connectionPool.items[conn] = false
	}
	return connectionPool
}

func (cp *ConnectionPool) Exchange(client *dns.Client, msg *dns.Msg) (r *dns.Msg, rtt time.Duration, err error) {
	conn := cp.getConnection()
	defer cp.releaseConnection(conn)
	return client.ExchangeWithConn(msg, conn)
}

func (cp *ConnectionPool) getConnection() *dns.Conn {
	getFreeConnection := func() *dns.Conn {
		cp.mutex.Lock()
		defer cp.mutex.Unlock()
		//log.Println("checking for free connection")
		for conn, locked := range cp.items {
			if !locked {
				cp.items[conn] = true
				//log.Println("found free connection")
				return conn
			}
		}
		//log.Println("didn't find free connection")
		return nil
	}
	conn := getFreeConnection()
	for conn == nil {
		//log.Println("waiting for free connection...")
		time.Sleep(time.Millisecond * 10)
		conn = getFreeConnection()
	}
	return conn
}

func (cp *ConnectionPool) releaseConnection(conn *dns.Conn) {
	cp.mutex.Lock()
	defer cp.mutex.Unlock()
	cp.items[conn] = false
}
