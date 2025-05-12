package main

import (
	"fmt"
	"github.com/Gilliam6/rtspv2_rtcp/format/rtspv2"
	"log"
	"os"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: rtspv2 <rtsp url>")
		os.Exit(1)
	}
	rtspUrl := os.Args[1]
	rtspClient, err := rtspv2.Dial(rtspv2.RTSPClientOptions{Debug: true, URL: rtspUrl, DialTimeout: 5 * time.Second, ReadWriteTimeout: 5 * time.Second})
	if err != nil {
		fmt.Println("RTSP Client Error:", err)
		os.Exit(1)
	}
	defer rtspClient.Close()

	//var start time.Time
	for {
		select {
		case pkt, ok := <-rtspClient.OutgoingPacketQueue:
			if !ok {
				fmt.Println("RTSP Client packet queue closed")
				os.Exit(0)
			}
			//if start.IsZero() {
			//	start = time.Now().Add(rtspClient.GetOffset())
			//}
			//cameraTime := time.Now().Add(rtspClient.GetOffset())
			//ntp := rtspClient.GetNTP()
			//log.Printf("NTP Camera Offset: %v", offset)

			log.Printf("RTSP Client packet received: %v", pkt.Time)
		case signal := <-rtspClient.Signals:
			if signal == rtspv2.SignalStreamRTPStop {
				fmt.Println("RTSP Client stopped")
				os.Exit(0)
			}
		}
	}
}
