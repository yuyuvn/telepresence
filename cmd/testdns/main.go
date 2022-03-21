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
)

func usage() {
	fmt.Printf("Usage: %s [--lock] SERVER\n", os.Args[0])
	os.Exit(1)
}

func main() {
	withLock := pflag.Bool("lock", false, "Whether to use a lock around DNS requests")
	pflag.Parse()
	addr := pflag.Arg(0)
	if addr == "" {
		usage()
	}

	lock := sync.Mutex{}
	conn, err := dns.Dial("udp", net.JoinHostPort(addr, "53"))
	if err != nil {
		fmt.Printf("failed to create conn: %v", err)
		os.Exit(1)
	}
	dc := dns.Client{
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

				if *withLock {
					lock.Lock()
				}
				_, _, err := dc.ExchangeWithConn(msg, conn)
				if *withLock {
					lock.Unlock()
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
