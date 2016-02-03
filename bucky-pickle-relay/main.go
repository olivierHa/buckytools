// bucky-pickle-relay is simply designed to decode the Python Pickle
// objects used in the Graphite Pickle protocol and forward them as
// plaintext protocol metrics to an upstream carbon-relay.  In my case
// I've used carbon-c-relay for routing and hashing but most of the
// incoming data is encoded in the pickle format.
//
// Copyright 2015 42 Lines, Inc.
// Original Author: Jack Neely <jjneely@42lines.net>
//
// 7/23/2015
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"os"
	"os/signal"
	"strings"
	"time"
)

import pickle "github.com/jjneely/buckytools/pickle"

// Where we listen for incoming TCP connections
var bindTo string

// Upstream carbon-relay location to send plaintext protocol metrics
var carbonRelay string

// Debug boolean
var debug bool

// sendTimout is the TCP timeout for out going line proto connections.
// This size is controlled by the -s flag and is how we react to failures
// of the carbon-relay-like daemon we are sending to.
var sendTimeout int

// pickleTimeout is the TCP timeout set on incoming pickle proto connections
// when the connection is first accepted and after every successful pickle
// object is received.  This disconnects idle open TCP connections from
// your app(s).  This is set with the -t flag.
var pickleTimeout int

// maxPickleSize is the largest pickle data stream we will accept
var maxPickleSize int

// pickleQueueSize is the buffer size used for the channels interconnecting
// the stages of execution.
var pickleQueueSize int

// prefix is the string prepended to internally generated metrics to control
// where they live in Graphite
var prefix string

// metricInterval is the interval in seconds between reporting of internal
// metrics
var metricInterval time.Duration

// Internal metrics
var seenPickles int = 0
var seenMetrics int = 0
var sentMetrics int = 0

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: %s [options] upstream-relay:port\n\n", os.Args[0])
	fmt.Fprintf(os.Stderr, "The first argument must be the network location to forward\n")
	fmt.Fprintf(os.Stderr, "plaintext carbon metrics to.\n\n")
	flag.PrintDefaults()
	os.Exit(1)
}

func serveForever() chan []string {
	log.Printf("Starting bucky-pickle-relay on %s", bindTo)
	tcpAddr, err := net.ResolveTCPAddr("tcp", bindTo)
	if err != nil {
		log.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp", tcpAddr)
	if err != nil {
		log.Fatal(err)
	}

	// Set up channel on which to send signal notifications.
	// We must use a buffered channel or risk missing the signal
	// if we're not ready to receive when the signal is sent.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, os.Kill)

	c := make(chan []string, pickleQueueSize)

	go func() {
		defer close(c)
		defer ln.Close()
		timer := time.Tick(metricInterval * time.Second)
		for {
			select {
			case <-timer:
				reportMetrics(c)
			case <-sig:
				// we are done, terminate goroutine
				log.Printf("Signal received, shutting down...")
				return
			default:
			}
			ln.SetDeadline(time.Now().Add(time.Second))
			conn, err := ln.Accept()
			if err, ok := err.(net.Error); ok && err.Timeout() {
				// Deadline timeout, continue loop
				continue
			} else if err != nil {
				log.Printf("Error accepting connection: %s", err)
				// Yield execution, we can't accept new connections anyway
				time.Sleep(time.Second)
				continue
			}

			go handleConn(c, conn)
		}

	}()

	return c
}

// reportMetrics adds internal metrics to the data stream, by adding a magic
// number to the byte slice that we look for to distinguish pickles.
func reportMetrics(c chan []string) {
	timestamp := time.Now().Unix()
	format := "%s%s %d %d"
	m := make([]string, 3)

	m[0] = fmt.Sprintf(format, prefix, ".seenPickles", seenPickles, timestamp)
	m[1] = fmt.Sprintf(format, prefix, ".seenMetrics", seenMetrics, timestamp)
	m[2] = fmt.Sprintf(format, prefix, ".sentMetrics", sentMetrics, timestamp)

	c <- m
}

func readSlice(conn net.Conn, buf []byte) error {
	for read := 0; read < len(buf); {
		n, err := conn.Read(buf[read:])
		read = read + n
		if err != nil {
			return err
		}
	}

	return nil
}

func handleConn(c chan []string, conn net.Conn) {
	if debug {
		log.Printf("Connection from %s", conn.RemoteAddr().String())
	}
	defer conn.Close()

	var size int
	var sizeBuf = make([]byte, 4)

	for {
		conn.SetDeadline(time.Now().Add(time.Second * time.Duration(pickleTimeout)))

		// Pickle is preceded by an unsigned long integer of 4 bytes (!L)
		err := readSlice(conn, sizeBuf)
		if err == io.EOF {
			// Remote end closed connection
			return
		} else if neterr, ok := err.(*net.OpError); ok && strings.Contains(neterr.Error(), "connection reset by peer") {
			// XXX: This used to work in Go 1.4 neterr.Err == syscall.ECONNRESET
			// Connection reset by peer between Pickles
			// or TCP probe health check
			// at this point in the proto we ignore
			return
		} else if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
			// Timeout waiting for data on connection
			log.Printf("Timeout on idle connection from: %s", conn.RemoteAddr())
			return
		} else if err != nil {
			log.Printf("Error reading connection from %s: %s",
				conn.RemoteAddr(), err)
			return
		}
		size = int(binary.BigEndian.Uint32(sizeBuf))

		if size > maxPickleSize {
			log.Printf("%s attempting to send %d bytes and is too large, aborting",
				conn.RemoteAddr(), size)
			return
		}

		// Allocate a buffer to read in the pickle data
		dataBuf := make([]byte, size)
		// Now read the pickle data
		err = readSlice(conn, dataBuf)
		if err != nil {
			log.Printf("Error reading pickle from %s: %s",
				conn.RemoteAddr(), err)
			return
		}

		seenPickles++
		metrics := decodePickle(dataBuf)
		if metrics != nil && len(metrics) > 0 {
			c <- metrics
		}
	}
}

func decodePickle(buff []byte) []string {
	metrics := make([]string, 0)

	decoder := pickle.NewDecoder(bytes.NewBuffer(buff))
	object, err := decoder.Decode()
	if err != nil {
		log.Printf("Error decoding pickle: %s", err)
		return nil
	}

	// Is this a slice -- it should be
	slice, ok := object.([]interface{})
	if !ok {
		log.Printf("Dropping pickle object: Should be []interface{} and is %T", object)
		return nil
	}

	for _, v := range slice {
		var key, ts, dp string
		metric, ok := v.([]interface{})
		if !ok {
			log.Printf("Dropping metric: []interface{} not data type inside pickle slice, rather %T", v)
			continue
		}

		key, ok = metric[0].(string)
		if !ok {
			log.Printf("Dropping metric: Unexpected %T type where metric key string should be", metric[0])
			continue
		}

		datatuple, ok := metric[1].([]interface{})
		if !ok {
			log.Printf("Dropping metric: ts, dp []interface{} not found, rather %T", metric[1])
			continue
		}

		switch t := datatuple[0].(type) {
		default:
			log.Printf("Dropping metric: Unexpected type %T in timestamp for %s", datatuple[0], key)
			continue
		case string:
			ts = strings.TrimSpace(t)
		case int64:
			ts = fmt.Sprintf("%d", t)
		case float64:
			ts = fmt.Sprintf("%f", t)
		case *big.Int:
			ts = fmt.Sprintf("%d", t)
		}

		switch t := datatuple[1].(type) {
		default:
			log.Printf("Dropping metric: Unexpected type %T in value for %s", datatuple[1], key)
			continue
		case string:
			dp = strings.TrimSpace(t)
		case int64:
			dp = fmt.Sprintf("%d", t)
		case float64:
			dp = fmt.Sprintf("%.f", t)
		case *big.Int:
			dp = fmt.Sprintf("%d", t)
		}

		metrics = append(metrics, fmt.Sprintf("%s %s %s", key, dp, ts))
	}

	seenMetrics = seenMetrics + len(metrics)
	return metrics
}

// plainTextBatch take a slice of individual plaintext metric strings
// and returns a slice of strings with several metrics concatenated
// together with a newline separator.  This helps us send fewer larger
// packets to our target.
func plainTextBatch(metrics []string, size int) []string {
	var i int
	ret := make([]string, 0)

	for i = 0; i < len(metrics)-size; i = i + size {
		ret = append(ret, strings.Join(metrics[i:i+size], "\n"))
	}

	ret = append(ret, strings.Join(metrics[i:], "\n"))
	return ret
}

func getRelayConnection() net.Conn {
	// Watch for signals here too...
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, os.Kill)

	// Block until we have a connection or are signaled from the OS
	for {
		select {
		case <-c:
			log.Printf("Signal received while trying to connect to upstream")
			return nil
		default:
		}

		conn, err := net.Dial("tcp", carbonRelay)
		if err != nil {
			log.Printf("Error connecting to upstream %s: %s", carbonRelay, err)
			time.Sleep(time.Second)
		} else {
			return conn
		}
	}

	return nil
}

func plainTextOut(metrics <-chan []string) {
	var batch []string
	conn := getRelayConnection()

	for slice := range metrics {
		// Batching
		batch = plainTextBatch(slice, 100)
		for len(batch) != 0 {
			// XXX: Extending the timeout is fairly expensive, using the assumption
			// we are talking to localhost we should probably put this in the
			// outer for loop...or do we really need it at all?
			// conn.SetDeadline(time.Now().Add(time.Second * time.Duration(sendTimeout)))
			_, err := conn.Write([]byte(batch[0] + "\n"))
			// XXX: Do we need to check for short writes?

			if err == nil {
				// On success we get the next batch
				sentMetrics = sentMetrics + strings.Count(batch[0], "\n") + 1
				batch = batch[1:]
			} else {
				// next write will write the current batch again
				log.Printf("Error writing to TCP socket, re-connecting: %s", err)
				conn.Close()
				conn = getRelayConnection()
			}
		}
	}
}

func main() {
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}

	flag.StringVar(&prefix, "p", fmt.Sprintf("bucky-pickle-relay.%s", hostname),
		"Prefix for internally generated metrics.")
	flag.StringVar(&bindTo, "b", ":2004",
		"Address to bind to for incoming connections.")
	flag.BoolVar(&debug, "d", false,
		"Debug mode.")
	flag.IntVar(&pickleTimeout, "t", 300,
		"Timeout in seconds on incoming pickle protocol TCP connections.")
	flag.IntVar(&sendTimeout, "s", 30,
		"TCP timeout in seconds for outgoing line protocol connections.")
	flag.IntVar(&maxPickleSize, "x", 1*1024*1024,
		"Maximum pickle size accepted.")
	flag.IntVar(&pickleQueueSize, "q", 0,
		"Internal buffer sizes.")
	flag.DurationVar(&metricInterval, "i", 60,
		"Interval in seconds between reporting of internal metrics.")
	flag.Parse()
	if flag.NArg() != 1 {
		usage()
	}

	log.Printf("bucky-pickle-relay Copyright 2015 42 Lines, Inc.")
	carbonRelay = flag.Arg(0)
	log.Printf("Sending line protocol data to %s", carbonRelay)
	log.Printf("Reporting internal metrics under %s", prefix)

	metrics := serveForever()
	plainTextOut(metrics)
}
