package main

import (
	"errors"
	"github.com/cbeuw/GoQuiet/gqserver"
	"io"
	"log"
	"net"
	"os"
	"time"
)

type pipe interface {
	remoteToServer()
	serverToRemote()
	closePipe()
}

type ssPair struct {
	ss     net.Conn
	remote net.Conn
}

type webPair struct {
	webServer net.Conn
	remote    net.Conn
}

func (pair *webPair) closePipe() {
	go pair.webServer.Close()
	go pair.remote.Close()
}

func (pair *ssPair) closePipe() {
	go pair.ss.Close()
	go pair.remote.Close()
}

func (pair *webPair) serverToRemote() {
	_, err := io.Copy(pair.remote, pair.webServer)
	if err != nil {
		pair.closePipe()
	}
}

func (pair *webPair) remoteToServer() {
	for {
		_, err := io.Copy(pair.webServer, pair.remote)
		if err != nil {
			pair.closePipe()
			return
		}
	}
}

func (pair *ssPair) remoteToServer() {
	for {
		data, err := gqserver.ReadTillDrain(pair.remote)
		if err != nil {
			pair.closePipe()
			return
		}
		data = gqserver.PeelRecordLayer(data)
		_, err = pair.ss.Write(data)
		if err != nil {
			pair.closePipe()
			return
		}
	}
}

func (pair *ssPair) serverToRemote() {
	for {
		buf := make([]byte, 10240)
		i, err := io.ReadAtLeast(pair.ss, buf, 1)
		if err != nil {
			pair.closePipe()
			return
		}
		data := buf[:i]
		data = gqserver.AddRecordLayer(data, []byte{0x17}, []byte{0x03, 0x03})
		_, err = pair.remote.Write(data)
		if err != nil {
			pair.closePipe()
			return
		}
	}
}

func dispatchConnection(conn net.Conn, sta *gqserver.State) {
	goWeb := func(data []byte) {
		pair, err := makeWebPipe(conn, sta)
		if err != nil {
			log.Println(err)
			go conn.Close()
			return
		}
		pair.webServer.Write(data)
		go pair.remoteToServer()
		go pair.serverToRemote()
	}
	goSS := func() {
		pair, err := makeSSPipe(conn, sta)
		if err != nil {
			log.Fatal(err)
		}
		go pair.remoteToServer()
		go pair.serverToRemote()
	}
	buf := make([]byte, 1500)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	i, err := io.ReadAtLeast(conn, buf, 1)
	if err != nil {
		go conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	data := buf[:i]
	ch, err := gqserver.ParseClientHello(data)
	if err != nil {
		goWeb(data)
		return
	}
	isSS := gqserver.IsSS(ch, sta)
	if !isSS {
		log.Printf("+1 non SS traffic from %v\n", conn.RemoteAddr())
		goWeb(data)
		return
	}
	reply := gqserver.ComposeReply(ch)
	_, err = conn.Write(reply)
	if err != nil {
		log.Println(err)
		go conn.Close()
		return
	}
	// Two discarded messages: ChangeCipherSpec and Finished
	for c := 0; c < 2; c++ {
		_, err = gqserver.ReadTillDrain(conn)
		if err != nil {
			log.Println(err)
			go conn.Close()
			return
		}
	}
	goSS()
}

func makeWebPipe(remote net.Conn, sta *gqserver.State) (*webPair, error) {
	conn, err := net.Dial("tcp", sta.WebServerAddr)
	if err != nil {
		return &webPair{}, errors.New("Connection to web server failed")
	}
	pair := &webPair{
		conn,
		remote,
	}
	return pair, nil
}

func makeSSPipe(remote net.Conn, sta *gqserver.State) (*ssPair, error) {
	conn, err := net.Dial("tcp", sta.SS_LOCAL_HOST+":"+sta.SS_LOCAL_PORT)
	if err != nil {
		return &ssPair{}, errors.New("Connection to SS server failed")
	}
	pair := &ssPair{
		conn,
		remote,
	}
	return pair, nil
}

func usedRandomCleaner(sta *gqserver.State) {
	for {
		time.Sleep(12 * time.Hour)
		now := int(sta.Now().Unix())
		for key, t := range sta.UsedRandom {
			if now-t > 12*3600 {
				sta.DelUsedRandom(key)
			}
		}
	}
}

func main() {
	sta := &gqserver.State{
		SS_LOCAL_HOST: os.Getenv("SS_LOCAL_HOST"),
		// Should be 127.0.0.1 unless the plugin and shadowsocks server are on seperate machines, which is not supported yet
		SS_LOCAL_PORT: os.Getenv("SS_LOCAL_PORT"),
		// SS loopback port, default set by SS to 8388
		SS_REMOTE_HOST: os.Getenv("SS_REMOTE_HOST"),
		// Outbound listening address, should be 0.0.0.0
		SS_REMOTE_PORT: os.Getenv("SS_REMOTE_PORT"),
		// Port exposed to the internet. Since this is a TLS obfuscator, this should be 443
		Now:        time.Now,
		UsedRandom: map[[32]byte]int{},
	}
	configPath := os.Getenv("SS_PLUGIN_OPTIONS")
	err := sta.ParseConfig(configPath)
	if err != nil {
		log.Fatalf("Configuration file error: %v", err)
	}
	sta.SetAESKey()
	go usedRandomCleaner(sta)
	listener, err := net.Listen("tcp", sta.SS_REMOTE_HOST+":"+sta.SS_REMOTE_PORT)
	log.Println("Listening on " + sta.SS_REMOTE_HOST + ":" + sta.SS_REMOTE_PORT)
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("%v", err)
			continue
		}
		go dispatchConnection(conn, sta)
	}

}
