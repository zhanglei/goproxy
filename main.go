package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
)

const APP_VERSION = "2.2"

var (
	checker           Checker
	proxyIsTls        bool
	localIsTls        bool
	proxyAddr         string
	isTCP             bool
	connTimeout       int
	certBytes         []byte
	keyBytes          []byte
	err               error
	outPool           ConnPool
	basicAuth         BasicAuth
	httpAuthorization bool
)

func init() {
	err = initConfig()
	if err != nil {
		log.Printf("err : %s", err)
		return
	}
}
func main() {
	//catch panic error
	defer func() {
		e := recover()
		if e != nil {
			log.Printf("err : %s,\ntrace:%s", e, string(debug.Stack()))
		}
	}()
	//define command line args
	proxyIsTls = cfg.GetBool("parent-tls")
	localIsTls = cfg.GetBool("local-tls")
	proxyAddr = cfg.GetString("parent")
	bindIP := cfg.GetString("ip")
	bindPort := cfg.GetInt("port")
	timeout := cfg.GetInt("check-timeout")
	connTimeout = cfg.GetInt("tcp-timeout")
	interval := cfg.GetInt("check-interval")
	certFile := cfg.GetString("cert")
	keyFile := cfg.GetString("key")
	blockedFile := cfg.GetString("blocked")
	directFile := cfg.GetString("direct")

	isTCP = cfg.GetBool("tcp")
	poolInitSize := cfg.GetInt("pool-size")

	//check args required
	if proxyIsTls && proxyAddr == "" {
		log.Fatalf("parent proxy address required")
	}

	//check tls cert&key file
	if certFile == "" {
		certFile = "proxy.crt"
	}
	if keyFile == "" {
		keyFile = "proxy.key"
	}
	if proxyIsTls || localIsTls {
		certBytes, err = ioutil.ReadFile(certFile)
		if err != nil {
			log.Printf("err : %s", err)
			return
		}
		keyBytes, err = ioutil.ReadFile(keyFile)
		if err != nil {
			log.Printf("err : %s", err)
			return
		}
	}
	//init tls info string
	var proxyIsTlsStr string
	var localIsTlsStr string
	protocolStr := "tcp"
	if !isTCP {
		protocolStr = "http(s)"
	}
	if proxyIsTls {
		proxyIsTlsStr = "tls "
	}
	if localIsTls {
		localIsTlsStr = "tls "
	}
	//init checker and pool if needed
	if proxyAddr != "" {
		if !isTCP && !cfg.GetBool("always") {
			checker = NewChecker(timeout, int64(interval), blockedFile, directFile)
		}
		log.Printf("use %sparent %s proxy : %s", proxyIsTlsStr, protocolStr, proxyAddr)
		initOutPool(proxyIsTls, certBytes, keyBytes, proxyAddr, connTimeout, poolInitSize, poolInitSize*2)
	} else if isTCP {
		log.Printf("tcp proxy need parent")
		return
	}
	//init basic auth only in http mode
	if !isTCP {
		basicAuth = NewBasicAuth()
		if cfg.GetString("auth-file") != "" {
			httpAuthorization = true
			n, err := basicAuth.AddFromFile(cfg.GetString("auth-file"))
			if err != nil {
				log.Fatalf("auth-file:%s", err)
			}
			log.Printf("auth data added from file %d , total:%d", n, basicAuth.Total())
		}
		if len(cfg.GetStringSlice("auth")) > 0 {
			httpAuthorization = true
			n := basicAuth.Add(cfg.GetStringSlice("auth"))
			log.Printf("auth data added %d, total:%d", n, basicAuth.Total())
		}
	}

	//listen
	sc := NewServerChannel(bindIP, bindPort)
	var err error
	if localIsTls {
		err = sc.ListenTls(certBytes, keyBytes, connHandler)
	} else {
		err = sc.ListenTCP(connHandler)
	}
	//listen fail
	if err != nil {
		log.Fatalf("ERR:%s", err)
	} else {
		log.Printf("%s %sproxy on %s", protocolStr, localIsTlsStr, (*sc.Listener).Addr())
	}
	//block main()
	signalChan := make(chan os.Signal, 1)
	cleanupDone := make(chan bool)
	signal.Notify(signalChan,
		os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		for _ = range signalChan {
			if outPool != nil {
				fmt.Println("\nReceived an interrupt, stopping services...")
				outPool.ReleaseAll()
				//time.Sleep(time.Second * 10)
				// fmt.Println("done")
			}
			cleanupDone <- true
		}
	}()
	<-cleanupDone
}
func connHandler(inConn net.Conn) {
	defer func() {
		err := recover()
		if err != nil {
			log.Printf("connHandler crashed,err:%s\nstack:%s", err, string(debug.Stack()))
			closeConn(&inConn)
		}
	}()
	if isTCP {
		tcpHandler(&inConn)
	} else {
		httpHandler(&inConn)
	}
}

func tcpHandler(inConn *net.Conn) {
	var outConn net.Conn
	var _outConn interface{}
	_outConn, err = outPool.Get()
	if err != nil {
		log.Printf("connect to %s , err:%s", proxyAddr, err)
		closeConn(inConn)
		return
	}
	outConn = _outConn.(net.Conn)
	inAddr := (*inConn).RemoteAddr().String()
	outAddr := outConn.RemoteAddr().String()
	IoBind((*inConn), outConn, func(err error) {
		log.Printf("conn %s - %s released", inAddr, outAddr)
		closeConn(inConn)
		closeConn(&outConn)
	}, func(n int, d bool) {}, 0)
	log.Printf("conn %s - %s connected", inAddr, outAddr)
}
func httpHandler(inConn *net.Conn) {
	var b [4096]byte
	var n int
	n, err = (*inConn).Read(b[:])
	if err != nil {
		if err != io.EOF {
			log.Printf("read err:%s", err)
		}
		closeConn(inConn)
		return
	}
	var method, host, address string
	index := bytes.IndexByte(b[:], '\n')
	if index == -1 {
		log.Printf("data err:%s", string(b[:n])[:50])
		closeConn(inConn)
		return
	}

	fmt.Sscanf(string(b[:index]), "%s%s", &method, &host)
	if method == "" || host == "" {
		log.Printf("data err:%s", string(b[:n])[:50])
		closeConn(inConn)
		return
	}
	isHTTPS := method == "CONNECT"

	//http basic auth,only http
	if !isHTTPS {
		if httpAuthorization {
			//log.Printf("request :%s", string(b[:n]))
			authorization, err := getHeader("Authorization", b[:n])
			if err != nil {
				fmt.Fprint(*inConn, "HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Basic realm=\"\"\r\n\r\nUnauthorized")
				closeConn(inConn)
				return
			}
			//log.Printf("Authorization:%s", authorization)
			basic := strings.Fields(authorization)
			if len(basic) != 2 {
				log.Printf("authorization data error,ERR:%s", authorization)
				closeConn(inConn)
				return
			}
			user, err := base64.StdEncoding.DecodeString(basic[1])
			if err != nil {
				log.Printf("authorization data parse error,ERR:%s", err)
				closeConn(inConn)
				return
			}
			authOk := basicAuth.Check(string(user))
			//log.Printf("auth %s,%v", string(user), authOk)
			if !authOk {
				fmt.Fprint(*inConn, "HTTP/1.1 401 Unauthorized\r\n\r\nUnauthorized")
				closeConn(inConn)
				return
			}
		}
	}

	var bytes []byte
	if isHTTPS { //https访问
		// [dd:dafds:fsd:dasd:2.2.23.3] or 2.2.23.3 or [dd:dafds:fsd:dasd:2.2.23.3]:2323 or 2.2.23.3:1234
		address = fixHost(host)
		if hostIsNoPort(host) { //host不带端口， 默认443
			address = address + ":443"
		}
	} else { //http访问
		hostPortURL, err := url.Parse(host)
		if err != nil {
			log.Printf("url.Parse %s ERR:%s", host, err)
			closeConn(inConn)
			return
		}
		_host := fixHost(hostPortURL.Host)
		address = _host
		if hostIsNoPort(_host) { //host不带端口， 默认80
			address = _host + ":80"
		}
		if _host != hostPortURL.Host {
			bytes = []byte(strings.Replace(string(b[:n]), hostPortURL.Host, _host, 1))
			host = strings.Replace(host, hostPortURL.Host, _host, 1)
		}
	}
	//get url , reslut host is the full url
	host, err = getURL(b[:n], host)
	// log.Printf("body:%s", string(b[:n]))
	// log.Printf("%s:%s", method, host)
	if err != nil {
		log.Printf("header data err:%s", err)
		closeConn(inConn)
		return
	}

	useProxy := false
	if proxyAddr != "" {
		if cfg.GetBool("always") {
			useProxy = true
		} else {
			if isHTTPS {
				checker.Add(address, true, method, "", nil)
			} else {
				if bytes != nil {
					checker.Add(address, false, method, host, bytes)
				} else {
					checker.Add(address, false, method, host, b[:n])
				}
			}
			useProxy, _, _ = checker.IsBlocked(address)
		}
		// var failN, successN uint
		// useProxy, failN, successN = checker.IsBlocked(address)
		//log.Printf("use proxy ? %s : %v ,fail:%d, success:%d", address, useProxy, failN, successN)
		//log.Printf("use proxy ? %s : %v", address, useProxy)
	}

	var outConn net.Conn
	var _outConn interface{}
	if useProxy {
		_outConn, err = outPool.Get()
		if err == nil {
			outConn = _outConn.(net.Conn)
		}
	} else {
		outConn, err = ConnectHost(address, connTimeout)
	}
	if err != nil {
		log.Printf("connect to %s , err:%s", address, err)
		closeConn(inConn)
		return
	}
	inAddr := (*inConn).RemoteAddr().String()
	outAddr := outConn.RemoteAddr().String()

	if isHTTPS {
		if useProxy {
			outConn.Write(b[:n])
		} else {
			fmt.Fprint(*inConn, "HTTP/1.1 200 Connection established\r\n\r\n")
		}
	} else {
		if bytes != nil {
			outConn.Write(bytes)
		} else {
			outConn.Write(b[:n])
		}
	}
	IoBind(*inConn, outConn, func(err error) {
		log.Printf("conn %s - %s [%s] released", inAddr, outAddr, address)
		closeConn(inConn)
		closeConn(&outConn)
	}, func(n int, d bool) {}, 0)
	log.Printf("conn %s - %s [%s] connected", inAddr, outAddr, address)
}
func closeConn(conn *net.Conn) {
	if *conn != nil {
		(*conn).SetDeadline(time.Now().Add(time.Millisecond))
		(*conn).Close()
	}
}
func keygen() (err error) {
	cmd := exec.Command("sh", "-c", "openssl genrsa -out proxy.key 2048")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("err:%s", err)
		return
	}
	fmt.Println(string(out))
	cmd = exec.Command("sh", "-c", `openssl req -new -key proxy.key -x509 -days 3650 -out proxy.crt -subj /C=CN/ST=BJ/O="Localhost Ltd"/CN=proxy`)
	out, err = cmd.CombinedOutput()
	if err != nil {
		log.Printf("err:%s", err)
		return
	}
	fmt.Println(string(out))
	return
}
