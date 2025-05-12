package rtspv2

import (
	"encoding/binary"
	"fmt"
	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/aacparser"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/codec/h265parser"
	"github.com/pion/rtcp"
	"log"
	"math"
	"sync"
	"time"
)

const (
	TimeBaseFactor = 90
	TimeDelay      = 1
	ntpEpochOffset = 2208988800
)

type ClockSync struct {
	mu        sync.Mutex
	inited    bool
	Start     time.Time
	Offset    time.Duration
	srNTP64   uint64  // last full 64-bit NTP time from SR
	srRTP     uint32  // last RTP timestamp from SR
	clockRate float64 // RTP clock frequency (e.g. 90000 for video)
}

func (c *ClockSync) Update(sr *rtcp.SenderReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.srNTP64 = sr.NTPTime
	c.srRTP = sr.RTPTime
	if !c.inited {
		c.Start = time.Now()
	}
	c.inited = true
	c.Offset = time.Now().Sub(ntp64ToTime(sr.NTPTime))
	log.Printf("ClockSync: NTP %v, RTP %v, offset %v, latency %f",
		ntp64ToTime(sr.NTPTime), sr.RTPTime, c.Offset)
}

// ComputeRTT computes the round-trip propagation delay using the
// 32-bit LSR and DLSR fields from an RTCP Receiver Report block.
//   - lsr   = Last SR timestamp (middle 32 bits of NTP) from the RR
//   - dlsr  = Delay since last SR (in 1/65536 sec units) from the RR
//   - recvT = local time at which this RR block was received
func (c *ClockSync) ComputeRTT(lsr, dlsr uint32, recvT time.Time) time.Duration {
	// 1) build a “short” NTP (16.16) from recvT
	unixSec := uint64(recvT.Unix())
	ntpSec := uint32(unixSec + ntpEpochOffset)
	ntpFrac := uint32(uint64(recvT.Nanosecond()) * (1 << 32) / 1e9)

	// take the middle 32 bits: low16(ntpSec) ∥ high16(ntpFrac)
	secLow := ntpSec & 0xFFFF
	fracHigh := ntpFrac >> 16
	recvShort := (secLow << 16) | fracHigh

	// 2) RTT_short = recvShort - lsr - dlsr  (wraps modulo 2³²)
	rttShort := recvShort - lsr - dlsr

	// 3) split back out to seconds + fraction
	secs := rttShort >> 16
	frac := rttShort & 0xFFFF
	nanos := (uint64(frac) * 1_000_000_000) >> 16

	return time.Duration(secs)*time.Second + time.Duration(nanos)
}

func (c *ClockSync) HandleRR(rr *rtcp.ReceiverReport) {
	for _, block := range rr.Reports {
		// block.LastSenderReport and block.Delay are your LSR, DLSR
		rtt := c.ComputeRTT(block.LastSenderReport, block.Delay, time.Now())
		log.Printf("RTT to SSRC %d = %v (loss=%d%%)", block.SSRC, rtt, block.FractionLost)
	}
}

func ntp64ToTime(ntp uint64) time.Time {
	sec := int64(ntp >> 32)
	frac := int64(ntp & 0xFFFFFFFF)
	nsec := (frac * 1e9) >> 32
	return time.Unix(sec-ntpEpochOffset, nsec)
}

// ... rest of your existing code unchanged ...

func (client *RTSPClient) RTPDemuxer(payloadRAW *[]byte) ([]*av.Packet, bool) {
	content := *payloadRAW
	firstByte := content[4]
	padding := (firstByte>>5)&1 == 1
	extension := (firstByte>>4)&1 == 1
	CSRCCnt := int(firstByte & 0x0f)
	client.sequenceNumber = int(binary.BigEndian.Uint16(content[6:8]))
	client.timestamp = int64(binary.BigEndian.Uint32(content[8:16]))

	if isRTCPPacket(content) {
		//client.Println("skipping RTCP packet")
		rtcpPkts, err := rtcp.Unmarshal(content[4:])
		if err != nil {
			client.Println("unmarshal rtcp packet error:", err)
			return nil, false
		}
		rtcpChan := int(content[1])
		for _, rtcpPkt := range rtcpPkts {
			if sr, ok := rtcpPkt.(*rtcp.SenderReport); ok {
				switch rtcpChan {
				case client.videoID + 1: // ← RTCP‑канал видео
					client.videoSync.Update(sr)
				case client.audioID + 1: // ← RTCP‑канал аудио
					client.audioSync.Update(sr)
				}
			} else if rr, ok := rtcpPkt.(*rtcp.ReceiverReport); ok {
				switch rtcpChan {
				case client.videoID + 1:
					client.videoSync.HandleRR(rr)
				case client.audioID + 1:
					client.audioSync.HandleRR(rr)
				}
			}
		}
		return nil, false
	}
	//lat := sync.Latency(client.timestamp, time.Now())
	//log.Printf("network delay: %v", lat)
	client.offset = RTPHeaderSize

	client.end = len(content)
	if client.end-client.offset >= 4*CSRCCnt {
		client.offset += 4 * CSRCCnt
	}
	if extension && len(content) < 4+client.offset+2+2 {
		return nil, false
	}
	if extension && client.end-client.offset >= 4 {
		extLen := 4 * int(binary.BigEndian.Uint16(content[4+client.offset+2:]))
		client.offset += 4
		if client.end-client.offset >= extLen {
			client.offset += extLen
		}
	}
	if padding && client.end-client.offset > 0 {
		paddingLen := int(content[client.end-1])
		if client.end-client.offset >= paddingLen {
			client.end -= paddingLen
		}
	}
	client.offset += 4
	if len(content) < client.end {
		return nil, false
	}

	switch int(content[1]) {
	case client.videoID:
		return client.handleVideo(content)
	case client.audioID:
		return client.handleAudio(content)
	}
	return nil, false
}

func (client *RTSPClient) handleVideo(content []byte) ([]*av.Packet, bool) {
	if client.PreVideoTS == 0 {
		client.PreVideoTS = client.timestamp
	}
	if client.timestamp-client.PreVideoTS < 0 {
		if math.MaxUint32-client.PreVideoTS < 90*100 { //100 ms
			client.PreVideoTS = 0
			client.PreVideoTS -= (math.MaxUint32 - client.PreVideoTS)
		} else {
			client.PreVideoTS = 0
		}
	}
	if client.PreSequenceNumber != 0 && client.sequenceNumber-client.PreSequenceNumber != 1 {
		client.Println("drop packet", client.sequenceNumber-1)
	}
	client.PreSequenceNumber = client.sequenceNumber
	if client.BufferRtpPacket.Len() > 4048576 {
		client.Println("Big Buffer Flush")
		client.BufferRtpPacket.Truncate(0)
		client.BufferRtpPacket.Reset()
	}
	nalRaw, _ := h264parser.SplitNALUs(content[client.offset:client.end])
	if len(nalRaw) == 0 || len(nalRaw[0]) == 0 {
		return nil, false
	}
	var retmap []*av.Packet
	for _, nal := range nalRaw {
		if client.videoCodec == av.H265 {
			retmap = client.handleH265Payload(nal, retmap)
		} else if client.videoCodec == av.H264 {
			retmap = client.handleH264Payload(content, nal, retmap)
		}
	}
	if len(retmap) > 0 {
		client.PreVideoTS = client.timestamp
		return retmap, true
	}

	return nil, false
}

func (client *RTSPClient) handleH264Payload(content, nal []byte, retmap []*av.Packet) []*av.Packet {
	naluType := nal[0] & 0x1f
	switch {
	case naluType >= 1 && naluType <= 5:
		retmap = client.appendVideoPacket(retmap, nal, naluType == 5)
	case naluType == h264parser.NALU_SPS:
		client.CodecUpdateSPS(nal)
	case naluType == h264parser.NALU_PPS:
		client.CodecUpdatePPS(nal)
	case naluType == 24:
		packet := nal[1:]
		for len(packet) >= 2 {
			size := int(packet[0])<<8 | int(packet[1])
			if size+2 > len(packet) {
				break
			}
			naluTypefs := packet[2] & 0x1f
			switch {
			case naluTypefs >= 1 && naluTypefs <= 5:
				retmap = client.appendVideoPacket(retmap, packet[2:size+2], naluTypefs == 5)
			case naluTypefs == h264parser.NALU_SPS:
				client.CodecUpdateSPS(packet[2 : size+2])
			case naluTypefs == h264parser.NALU_PPS:
				client.CodecUpdatePPS(packet[2 : size+2])
			}
			packet = packet[size+2:]
		}
	case naluType == 28:
		fuIndicator := content[client.offset]
		fuHeader := content[client.offset+1]
		isStart := fuHeader&0x80 != 0
		isEnd := fuHeader&0x40 != 0
		if isStart {
			client.fuStarted = true
			client.BufferRtpPacket.Truncate(0)
			client.BufferRtpPacket.Reset()
			client.BufferRtpPacket.Write([]byte{fuIndicator&0xe0 | fuHeader&0x1f})
		}
		if client.fuStarted {
			client.BufferRtpPacket.Write(content[client.offset+2 : client.end])
			if isEnd {
				client.fuStarted = false
				naluTypef := client.BufferRtpPacket.Bytes()[0] & 0x1f
				if naluTypef == 7 || naluTypef == 9 {
					bufered, _ := h264parser.SplitNALUs(append([]byte{0, 0, 0, 1}, client.BufferRtpPacket.Bytes()...))
					for _, v := range bufered {
						naluTypefs := v[0] & 0x1f
						switch {
						case naluTypefs == 5:
							client.BufferRtpPacket.Reset()
							client.BufferRtpPacket.Write(v)
							naluTypef = 5
						case naluTypefs == h264parser.NALU_SPS:
							client.CodecUpdateSPS(v)
						case naluTypefs == h264parser.NALU_PPS:
							client.CodecUpdatePPS(v)
						}
					}
				}
				retmap = client.appendVideoPacket(retmap, client.BufferRtpPacket.Bytes(), naluTypef == 5)
			}
		}
	default:
		//client.Println("Unsupported NAL Type", naluType)
	}

	return retmap
}

func (client *RTSPClient) handleH265Payload(nal []byte, retmap []*av.Packet) []*av.Packet {
	naluType := (nal[0] >> 1) & 0x3f
	switch naluType {
	case h265parser.NAL_UNIT_CODED_SLICE_TRAIL_R:
		retmap = client.appendVideoPacket(retmap, nal, false)
	case h265parser.NAL_UNIT_VPS:
		client.CodecUpdateVPS(nal)
	case h265parser.NAL_UNIT_SPS:
		client.CodecUpdateSPS(nal)
	case h265parser.NAL_UNIT_PPS:
		client.CodecUpdatePPS(nal)
	case h265parser.NAL_UNIT_UNSPECIFIED_49:
		se := nal[2] >> 6
		naluType := nal[2] & 0x3f
		switch se {
		case 2:
			client.BufferRtpPacket.Truncate(0)
			client.BufferRtpPacket.Reset()
			client.BufferRtpPacket.Write([]byte{(nal[0] & 0x81) | (naluType << 1), nal[1]})
			r := make([]byte, 2)
			r[1] = nal[1]
			r[0] = (nal[0] & 0x81) | (naluType << 1)
			client.BufferRtpPacket.Write(nal[3:])
		case 1:
			client.BufferRtpPacket.Write(nal[3:])
			retmap = client.appendVideoPacket(retmap, client.BufferRtpPacket.Bytes(), naluType == h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL)
		default:
			client.BufferRtpPacket.Write(nal[3:])
		}
	default:
		//client.Println("Unsupported Nal", naluType)
	}
	return retmap
}

func (client *RTSPClient) handleAudio(content []byte) ([]*av.Packet, bool) {
	if client.PreAudioTS == 0 {
		client.PreAudioTS = client.timestamp
	}
	nalRaw, _ := h264parser.SplitNALUs(content[client.offset:client.end])
	var retmap []*av.Packet
	for _, nal := range nalRaw {
		var duration time.Duration
		switch client.audioCodec {
		case av.PCM_MULAW, av.PCM_ALAW:
			duration = time.Duration(len(nal)) * time.Second / time.Duration(client.AudioTimeScale)
			retmap = client.appendAudioPacket(retmap, nal, duration)
		case av.OPUS:
			duration = time.Duration(20) * time.Millisecond
			retmap = client.appendAudioPacket(retmap, nal, duration)
		case av.AAC:
			auHeadersLength := uint16(0) | (uint16(nal[0]) << 8) | uint16(nal[1])
			auHeadersCount := auHeadersLength >> 4
			framesPayloadOffset := 2 + int(auHeadersCount)<<1
			auHeaders := nal[2:framesPayloadOffset]
			framesPayload := nal[framesPayloadOffset:]
			for i := 0; i < int(auHeadersCount); i++ {
				auHeader := uint16(0) | (uint16(auHeaders[0]) << 8) | uint16(auHeaders[1])
				frameSize := auHeader >> 3
				frame := framesPayload[:frameSize]
				auHeaders = auHeaders[2:]
				framesPayload = framesPayload[frameSize:]
				if _, _, _, _, err := aacparser.ParseADTSHeader(frame); err == nil {
					frame = frame[7:]
				}
				duration = time.Duration((float32(1024)/float32(client.AudioTimeScale))*1000*1000*1000) * time.Nanosecond
				retmap = client.appendAudioPacket(retmap, frame, duration)
			}
		}
	}
	if len(retmap) > 0 {
		client.PreAudioTS = client.timestamp
		return retmap, true
	}
	return nil, false
}

func (client *RTSPClient) appendAudioPacket(retmap []*av.Packet, nal []byte, duration time.Duration) []*av.Packet {
	client.AudioTimeLine += duration
	fmt.Printf("%v\n", client.AudioTimeLine-time.Since(client.audioSync.Start))
	return append(retmap, &av.Packet{
		Data:            nal,
		CompositionTime: time.Duration(1) * time.Millisecond,
		Duration:        duration,
		Idx:             client.audioIDX,
		IsKeyFrame:      false,
		Time:            client.AudioTimeLine,
	})
}

func (client *RTSPClient) appendVideoPacket(retmap []*av.Packet, nal []byte, isKeyFrame bool) []*av.Packet {
	fmt.Printf("%v\n", time.Duration(client.timestamp/TimeBaseFactor)*time.Millisecond-time.Since(client.videoSync.Start))

	return append(retmap, &av.Packet{
		Data:            append(binSize(len(nal)), nal...),
		CompositionTime: time.Duration(TimeDelay) * time.Millisecond,
		Idx:             client.videoIDX,
		IsKeyFrame:      isKeyFrame,
		Duration:        time.Duration(float32(client.timestamp-client.PreVideoTS)/TimeBaseFactor) * time.Millisecond,
		Time:            time.Duration(client.timestamp/TimeBaseFactor) * time.Millisecond,
	})
}
