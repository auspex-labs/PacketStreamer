package streamer

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/deepfence/PacketStreamer/pkg/config"
)

const (
	compressFlagByteLen = 1
	payloadMarkerLen    = 4
	maxNumPkts          = 100
	connTimeout         = 60
)

var (
	hdrData = [...]byte{0xde, 0xef, 0xec, 0xe0}
)

func readDataFromSocket(hostConn net.Conn, dataBuff []byte, bytesToRead int) error {

	totalBytesRead := 0

	for {
		deadLineErr := hostConn.SetReadDeadline(time.Now().Add(connTimeout * time.Second))
		if deadLineErr != nil {
			log.Println(fmt.Sprintf("Unable to set timeout for connection from %s. Reason %v",
				hostConn.RemoteAddr(), deadLineErr))
		}
		bytesRead, readErr := hostConn.Read(dataBuff[totalBytesRead:])
		if (readErr != nil) && (readErr != io.EOF) && !os.IsTimeout(readErr) {
			return fmt.Errorf("Client %s closed connection. Reason = %v", hostConn.RemoteAddr(), readErr)
		}
		if (readErr == io.EOF) && (totalBytesRead != bytesToRead) {
			return fmt.Errorf("Client %s closed connection abruptly. Got EOF", hostConn.RemoteAddr())
		}
		if os.IsTimeout(readErr) && (totalBytesRead != bytesToRead) {
			return readErr
		}
		if (bytesRead == 0) && (readErr != nil) {
			log.Printf("Zero bytes received from client. Error reason %v\n", readErr)
			return nil
		}
		if (bytesRead == 0) && (readErr == nil) {
			log.Println("Zero bytes received from client. No errors")
			return nil
		}
		totalBytesRead += bytesRead
		if totalBytesRead == bytesToRead {
			return nil
		}
	}
}

func readPkts(clientConn net.Conn, config *config.Config, pktUncompressChannel chan CompressData, sizeChannel chan int) {

	var dataBuff = make([]byte, config.CompressBlockSize * kilobyte)
	var totalHdrLen = len(hdrData) + payloadMarkerLen + compressFlagByteLen

	for {
		err := readDataFromSocket(clientConn, dataBuff[0:totalHdrLen], totalHdrLen)
		if err != nil {
			if !os.IsTimeout(err) {
				log.Printf("Unable to read data from connection. %v\n", err)
			}
			clientConn.Close()
			close(pktUncompressChannel)
			return
		}
		compareRes := bytes.Compare(dataBuff[0:len(hdrData)], hdrData[:])
		if compareRes != 0 {
			log.Printf("Illegal data received from client")
			clientConn.Close()
			close(pktUncompressChannel)
			return
		}
		compressedDataLen := binary.LittleEndian.Uint32(dataBuff[len(hdrData):])
		if int(compressedDataLen) > ((config.CompressBlockSize * kilobyte) - totalHdrLen) {
			log.Printf("Invalid buffer length %d obtained from client", compressedDataLen)
			clientConn.Close()
			close(pktUncompressChannel)
			return
		}
		err = readDataFromSocket(clientConn, dataBuff[totalHdrLen:(totalHdrLen+int(compressedDataLen))], int(compressedDataLen))
		if err != nil {
			log.Printf("Unable to read data from connection. %s\n", err)
			clientConn.Close()
			close(pktUncompressChannel)
			return
		}
		dataToUncompress := CompressData{
			Data: string(dataBuff[totalHdrLen:(int(compressedDataLen) + totalHdrLen)]),
			IsCompressed: dataBuff[len(hdrData)+payloadMarkerLen] != 0,
		}
		select {
		case pktUncompressChannel <- dataToUncompress:
		default:
			log.Println("Uncompress queue is full. Discarding")
		}
		select {
		case sizeChannel <- (totalHdrLen + int(compressedDataLen)):
		default:
			log.Println("Size queue is full. Discarding")
		}
	}
}

func receiverOutput(config *config.Config, consolePktOutputChannel chan string) {

	for {
		tmpData, chanExitVal := <-consolePktOutputChannel

		if !chanExitVal {
			log.Println("Error while reading from output channel")
			break
		}

		if writeOutput(config, []byte(tmpData)) == 1 {
			break
		}
	}
}

func processHost(config *config.Config, consolePktOutputChannel chan string, proto string) {

	var err error
	var listener net.Listener
	addr := config.Input.Address
	if config.Input.Port != nil {
		addr = fmt.Sprintf("%s:%d", config.Input.Address, *config.Input.Port)
	}

	if config.TLS.Enable {
		config, err := getTlsConfig(config.TLS.CertFile, config.TLS.KeyFile, "")
		if err != nil {
			log.Println("Unable to start TLS listener: "+err.Error())
			return
		}
		listener, err = tls.Listen(proto, addr, config)
		if err != nil {
			log.Println("Unable to start TLS listener socket "+err.Error(), proto, addr, config)
			return
		}
	} else {
		listener, err = net.Listen(proto, addr)
		if err != nil {
			log.Println("Unable to start listener socket "+err.Error(), proto, addr)
			return
		}
	}

	sizeChannel := make(chan int, maxNumPkts)
	go calculateDataSize(sizeChannel)

	for {
		hostConn, cerr := listener.Accept()
		if cerr != nil {
			log.Println("Unable to accept connections on socket " + cerr.Error())
			break
		} else {
			log.Println("Accepted connection on socket: ", proto, hostConn.RemoteAddr())
		}
		if config.Auth.Enable {
			go func() {
				if handleServerAuth(hostConn) {
					pktUncompressChannel := make(chan CompressData, maxNumPkts)
					go decompressPkts(config, pktUncompressChannel, consolePktOutputChannel)
					go readPkts(hostConn, config, pktUncompressChannel, sizeChannel)
				}
			}()
			continue
		}
		pktUncompressChannel := make(chan CompressData, maxNumPkts)
		go decompressPkts(config, pktUncompressChannel, consolePktOutputChannel)
		go readPkts(hostConn, config, pktUncompressChannel, sizeChannel)
	}
}

func StartReceiver(config *config.Config, proto string, mainSignalChannel chan bool) {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				printDataSize()
			}
		}
	}()
	consolePktOutputChannel := make(chan string, maxNumPkts*10)
	go receiverOutput(config, consolePktOutputChannel)
	go processHost(config, consolePktOutputChannel, proto)
}
