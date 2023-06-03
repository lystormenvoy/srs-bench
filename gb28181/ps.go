// The MIT License (MIT)
//
// # Copyright (c) 2022 Winlin
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
package gb28181

import (
	"context"
	"fmt"
	"github.com/ossrs/go-oryx-lib/errors"
	"github.com/pion/rtp"
	"github.com/yapingcat/gomedia/codec"
	"github.com/yapingcat/gomedia/mpeg2"
	"math"
	"net"
	"net/url"
	"strings"
)

type PSConfig struct {
	// The video source file.
	video string
	// The fps for h264 file.
	fps int
	// The audio source file.
	audio string
}

func (v *PSConfig) String() string {
	sb := []string{}
	if v.video != "" {
		sb = append(sb, fmt.Sprintf("video=%v", v.video))
	}
	if v.fps > 0 {
		sb = append(sb, fmt.Sprintf("fps=%v", v.fps))
	}
	if v.audio != "" {
		sb = append(sb, fmt.Sprintf("audio=%v", v.audio))
	}
	return strings.Join(sb, ",")
}

type PSClient struct {
	// SSRC from SDP.
	ssrc uint32
	// The server IP address and port to connect to.
	serverAddr string
	// Inner state, sequence number.
	seq uint16
	// Inner state, media TCP connection
	conn *net.TCPConn
}

func NewPSClient(ssrc uint32, serverAddr string) *PSClient {
	return &PSClient{ssrc: ssrc, serverAddr: serverAddr}
}

func (v *PSClient) Close() error {
	if v.conn != nil {
		v.conn.Close()
	}
	return nil
}

func (v *PSClient) Connect(ctx context.Context) error {
	if u, err := url.Parse(v.serverAddr); err != nil {
		return errors.Wrapf(err, "parse addr=%v", v.serverAddr)
	} else if addr, err := net.ResolveTCPAddr(u.Scheme, u.Host); err != nil {
		return errors.Wrapf(err, "parse addr=%v, scheme=%v, host=%v", v.serverAddr, u.Scheme, u.Host)
	} else if v.conn, err = net.DialTCP(u.Scheme, nil, addr); err != nil {
		return errors.Wrapf(err, "connect addr=%v as %v", v.serverAddr, addr.String())
	}

	return nil
}

func (v *PSClient) WritePacksOverRTP(packs []*PSPacket) error {
	for _, pack := range packs {
		for _, payload := range pack.ps {
			v.seq++
			p := rtp.Packet{Header: rtp.Header{
				Version: 2, PayloadType: uint8(pack.pt), SequenceNumber: v.seq,
				Timestamp: uint32(pack.ts), SSRC: uint32(v.ssrc),
			}, Payload: payload}

			b, err := p.Marshal()
			if err != nil {
				return errors.Wrapf(err, "rtp marshal")
			}

			if _, err = v.conn.Write([]byte{uint8(len(b) >> 8), uint8(len(b))}); err != nil {
				return errors.Wrapf(err, "write length=%v", len(b))
			}

			if _, err = v.conn.Write(b); err != nil {
				return errors.Wrapf(err, "write payload %v bytes", len(b))
			}
		}
	}

	return nil
}

type PSPacketType int

const (
	PSPacketTypePackHeader PSPacketType = iota
	PSPacketTypeSystemHeader
	PSPacketTypeProgramStramMap
	PSPacketTypeVideo
	PSPacketTypeAudio
)

type PSPacket struct {
	t  PSPacketType
	ts uint64
	pt uint8
	ps [][]byte
}

func NewPSPacket(t PSPacketType, p []byte, ts uint64, pt uint8) *PSPacket {
	v := &PSPacket{t: t, ts: ts, pt: pt}
	if p != nil {
		v.ps = append(v.ps, p)
	}
	return v
}

func (v *PSPacket) Append(p []byte) *PSPacket {
	v.ps = append(v.ps, p)
	return v
}

type PSPackStream struct {
	// The RTP paload type.
	pt uint8
	// Split a big media frame to small PES packets.
	ideaPesLength int
	// The generated bytes of PS stream data.
	packets []*PSPacket
	// Whether has video packet.
	hasVideo bool
}

func NewPSPackStream(pt uint8) *PSPackStream {
	return &PSPackStream{ideaPesLength: 1400, pt: pt}
}

func (v *PSPackStream) WriteHeader(videoCodec mpeg2.PS_STREAM_TYPE, dts uint64) error {
	if err := v.WritePackHeader(dts); err != nil {
		return err
	}
	if err := v.WriteSystemHeader(dts); err != nil {
		return err
	}
	if err := v.WriteProgramStreamMap(videoCodec, dts); err != nil {
		return err
	}
	return nil
}

func (v *PSPackStream) WritePackHeader(dts uint64) error {
	w := codec.NewBitStreamWriter(1500)

	pack := &mpeg2.PSPackHeader{
		System_clock_reference_base: dts,
		Program_mux_rate:            159953,
		Pack_stuffing_length:        6,
	}

	pack.Encode(w)

	v.packets = append(v.packets, NewPSPacket(PSPacketTypePackHeader, w.Bits(), dts, v.pt))
	return nil
}

func (v *PSPackStream) WriteSystemHeader(dts uint64) error {
	w := codec.NewBitStreamWriter(1500)

	system := &mpeg2.System_header{
		Rate_bound:  159953,
		Video_bound: 1,
		Audio_bound: 1,
		Streams: []*mpeg2.Elementary_Stream{
			// SrsTsPESStreamIdVideoCommon = 0xe0
			&mpeg2.Elementary_Stream{Stream_id: uint8(0xe0), P_STD_buffer_bound_scale: 1, P_STD_buffer_size_bound: 128},
			// SrsTsPESStreamIdAudioCommon = 0xc0
			&mpeg2.Elementary_Stream{Stream_id: uint8(0xc0), P_STD_buffer_bound_scale: 0, P_STD_buffer_size_bound: 8},
			// SrsTsPESStreamIdPrivateStream1 = 0xbd
			&mpeg2.Elementary_Stream{Stream_id: uint8(0xbd), P_STD_buffer_bound_scale: 1, P_STD_buffer_size_bound: 128},
			// SrsTsPESStreamIdPrivateStream2 = 0xbf
			&mpeg2.Elementary_Stream{Stream_id: uint8(0xbf), P_STD_buffer_bound_scale: 1, P_STD_buffer_size_bound: 128},
		},
	}

	system.Encode(w)

	v.packets = append(v.packets, NewPSPacket(PSPacketTypeSystemHeader, w.Bits(), dts, v.pt))
	return nil
}

func (v *PSPackStream) WriteProgramStreamMap(videoCodec mpeg2.PS_STREAM_TYPE, dts uint64) error {
	w := codec.NewBitStreamWriter(1500)

	psm := &mpeg2.Program_stream_map{
		Stream_map: []*mpeg2.Elementary_stream_elem{
			// SrsTsPESStreamIdVideoCommon = 0xe0
			mpeg2.NewElementary_stream_elem(uint8(videoCodec), 0xe0),
			// SrsTsPESStreamIdAudioCommon = 0xc0
			mpeg2.NewElementary_stream_elem(uint8(mpeg2.PS_STREAM_AAC), 0xc0),
		},
	}

	psm.Current_next_indicator = 1
	psm.Encode(w)

	v.packets = append(v.packets, NewPSPacket(PSPacketTypeProgramStramMap, w.Bits(), dts, v.pt))
	return nil
}

// The nalu is raw data without ANNEXB header.
func (v *PSPackStream) WriteVideo(nalu []byte, dts uint64) error {
	// Mux frame payload in AnnexB format. Always fresh NALU header for frame, see srs_avc_insert_aud.
	annexb := append([]byte{0, 0, 0, 1}, nalu...)

	video := NewPSPacket(PSPacketTypeVideo, nil, dts, v.pt)

	for i := 0; i < len(annexb); i += v.ideaPesLength {
		payloadLength := int(math.Min(float64(v.ideaPesLength), float64(len(annexb)-i)))
		bb := annexb[i : i+payloadLength]

		w := codec.NewBitStreamWriter(65535)

		pes := &mpeg2.PesPacket{
			Stream_id:     uint8(0xe0),                     // SrsTsPESStreamIdVideoCommon = 0xe0
			PTS_DTS_flags: uint8(0x03), Dts: dts, Pts: dts, // Both DTS and PTS.
			Pes_payload: bb,
		}
		utilUpdatePesPacketLength(pes)

		pes.Encode(w)

		video.Append(w.Bits())
	}

	v.hasVideo = true
	v.packets = append(v.packets, video)
	return nil
}

// Write AAC ADTS frame.
func (v *PSPackStream) WriteAudio(adts []byte, dts uint64) error {
	w := codec.NewBitStreamWriter(65535)

	pes := &mpeg2.PesPacket{
		Stream_id:     uint8(0xc0),                     // SrsTsPESStreamIdAudioCommon = 0xc0
		PTS_DTS_flags: uint8(0x03), Dts: dts, Pts: dts, // Both DTS and PTS.
		Pes_payload: adts,
	}
	utilUpdatePesPacketLength(pes)

	pes.Encode(w)

	v.packets = append(v.packets, NewPSPacket(PSPacketTypeAudio, w.Bits(), dts, v.pt))
	return nil
}
