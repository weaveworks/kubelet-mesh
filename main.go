package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"crypto/x509"
	"encoding/pem"

	"github.com/weaveworks/mesh"
)

func main() {
	peers := &stringset{}
	apiservers := &stringset{}
	var (
		meshListen = flag.String("mesh", net.JoinHostPort("0.0.0.0", strconv.Itoa(mesh.Port)), "mesh listen address")
		hwaddr     = flag.String("hwaddr", mustHardwareAddr(), "MAC address, i.e. mesh peer ID")
		nickname   = flag.String("nickname", mustHostname(), "peer nickname")
		password   = flag.String("password", "", "password (optional)")
		rootCA     = flag.String("root-ca", "", "root CA certificate")
	)
	flag.Var(peers, "peer", "initial peer (may be repeated)")
	flag.Var(apiservers, "apiserver", "the URL of the apiserver (may be repeated)")
	flag.Parse()

	logger := log.New(os.Stderr, *nickname+"> ", log.LstdFlags)

	host, portStr, err := net.SplitHostPort(*meshListen)
	if err != nil {
		logger.Fatalf("mesh address: %s: %v", *meshListen, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		logger.Fatalf("mesh address: %s: %v", *meshListen, err)
	}

	name, err := mesh.PeerNameFromString(*hwaddr)
	if err != nil {
		logger.Fatalf("%s: %v", *hwaddr, err)
	}

	certInfo := &RootCAPublicKey{}

	if *rootCA != "" {
		logger.Print("Found a certificate...")
		ca, err := ioutil.ReadFile(*rootCA)
		if err != nil {
			logger.Print(err)
		}

		certBlock, _ := pem.Decode(ca)
		cert, err := x509.ParseCertificate(certBlock.Bytes)
		if err != nil {
			logger.Print(err)
		}

		logger.Printf("Picked up root CA certificate which is not valid before %v", cert.NotBefore)
		certInfo.NotBefore = cert.NotBefore
		certInfo.Signature = cert.Signature
		certInfo.Bytes = certBlock.Bytes
	}

	router := mesh.NewRouter(mesh.Config{
		Host:               host,
		Port:               port,
		ProtocolMinVersion: mesh.ProtocolMinVersion,
		Password:           []byte(*password),
		ConnLimit:          64,
		PeerDiscovery:      true,
		TrustedSubnets:     []*net.IPNet{},
	}, name, *nickname, mesh.NullOverlay{}, log.New(ioutil.Discard, "", 0))

	// XXX change "node" to something else, "kubelet"?
	apiserverURLs := make([]string, 0)
	for _, apiserver := range apiservers.slice() {
		if apiserverURL, err := url.Parse(apiserver); err == nil {
			apiserverURLs = append(apiserverURLs, apiserverURL.String())
		} else {
			logger.Printf("Could not parse %q as ULR - %s", apiserver, err)
		}
	}

	nodeBootstrapPeer := newNodeBootstrapPeer(name, certInfo, apiserverURLs, logger)
	nodeBootstrap := router.NewGossip("kubernetes-node-bootstrap-v0", nodeBootstrapPeer)
	nodeBootstrapPeer.register(nodeBootstrap)

	func() {
		logger.Printf("mesh router starting (%s)", *meshListen)
		router.Start()
	}()
	defer func() {
		logger.Printf("mesh router stopping")
		router.Stop()
	}()

	router.ConnectionMaker.InitiateConnections(peers.slice(), true)

	errs := make(chan error)
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT)
		errs <- fmt.Errorf("%s", <-c)
	}()

	go func() {
		time.Sleep(5 * time.Second)
		logger.Print(mesh.NewStatus(router).Connections)
	}()

	logger.Print(<-errs)
}

type stringset map[string]struct{}

func (ss stringset) Set(value string) error {
	ss[value] = struct{}{}
	return nil
}

func (ss stringset) String() string {
	return strings.Join(ss.slice(), ",")
}

func (ss stringset) slice() []string {
	slice := make([]string, 0, len(ss))
	for k := range ss {
		slice = append(slice, k)
	}
	sort.Strings(slice)
	return slice
}

func mustHardwareAddr() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		panic(err)
	}
	for _, iface := range ifaces {
		if s := iface.HardwareAddr.String(); s != "" {
			return s
		}
	}
	panic("no valid network interfaces")
}

func mustHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return hostname
}
