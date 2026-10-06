package flv

import (
	"bufio"
	"fmt"
	"io"
	"sort"

	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/av/avutil"
	"github.com/datarhei/joy4/codec"
	"github.com/datarhei/joy4/codec/aacparser"
	"github.com/datarhei/joy4/codec/av1parser"
	"github.com/datarhei/joy4/codec/fake"
	"github.com/datarhei/joy4/codec/h264parser"
	"github.com/datarhei/joy4/codec/hevcparser"
	"github.com/datarhei/joy4/codec/vp9parser"
	"github.com/datarhei/joy4/codec/vvcparser"
	"github.com/datarhei/joy4/format/flv/flvio"
	"github.com/datarhei/joy4/utils/bits/pio"
)

var MaxProbePacketCount = 20

func NewMetadataByStreams(streams []av.CodecData) (metadata flvio.AMFMap, err error) {
	metadata = flvio.AMFMap{}

	for _, _stream := range streams {
		typ := _stream.Type()
		switch {
		case typ.IsVideo():
			stream := _stream.(av.VideoCodecData)
			switch typ {
			case av.H264:
				metadata["videocodecid"] = flvio.VIDEO_H264
			case av.HEVC:
				metadata["videocodecid"] = flvio.FourCCToFloat(flvio.FOURCC_HEVC)
			case av.VP9:
				metadata["videocodecid"] = flvio.FourCCToFloat(flvio.FOURCC_VP9)
			case av.AV1:
				metadata["videocodecid"] = flvio.FourCCToFloat(flvio.FOURCC_AV1)
			case av.VVC:
				metadata["videocodecid"] = flvio.FourCCToFloat(flvio.FOURCC_VVC)

			default:
				err = fmt.Errorf("flv: metadata: unsupported video codecType=%v", stream.Type())
				return
			}

			width, height := stream.Width(), stream.Height()

			if width != 0 {
				metadata["width"] = width
				metadata["displayWidth"] = width
			}

			if height != 0 {
				metadata["height"] = height
				metadata["displayHeight"] = height
			}

		case typ.IsAudio():
			stream := _stream.(av.AudioCodecData)
			switch typ {
			case av.AAC:
				metadata["audiocodecid"] = flvio.SOUND_AAC

			case av.SPEEX:
				metadata["audiocodecid"] = flvio.SOUND_SPEEX

			default:
				err = fmt.Errorf("flv: metadata: unsupported audio codecType=%v", stream.Type())
				return
			}

			metadata["audiosamplerate"] = stream.SampleRate()
		}
	}

	return
}

// ProbeQuietWindow is the number of pushed tags without a newly discovered
// track after which probing of an Enhanced RTMP multi-track audio stream is
// considered complete. Encoders emit all codec config tags up front, so a
// short quiet window reliably captures every track.
const ProbeQuietWindow = 4

type cachedTag struct {
	tag flvio.Tag
	ts  int64
}

type Prober struct {
	HasAudio, HasVideo             bool
	GotAudio, GotVideo             bool
	VideoStreamIdx, AudioStreamIdx int
	PushedCount                    int
	MaxProbePacketCount            int
	Streams                        []av.CodecData

	// AudioStreamIdxs maps the audio ordinal (ordered by wire track ID) to the
	// stream index within Streams. Valid after probing completed.
	AudioStreamIdxs []int

	// DroppedUnknownTrack counts media tags whose track ID was never
	// registered (e.g. a track appearing after probing completed).
	DroppedUnknownTrack int

	videoTracks   map[uint8]av.CodecData
	audioTracks   map[uint8]av.CodecData
	videoOrder    []uint8
	audioOrder    []uint8
	videoTrackIdx map[uint8]int8
	audioTrackIdx map[uint8]int8
	frozen        bool

	lastNewTrackPush int
	cachedTags       []cachedTag
}

func NewProber(maxProbePacketCount int) *Prober {
	prober := &Prober{
		MaxProbePacketCount: maxProbePacketCount,
	}

	return prober
}

func (prober *Prober) registerVideoTrack(trackID uint8, stream av.CodecData) {
	if prober.videoTracks == nil {
		prober.videoTracks = make(map[uint8]av.CodecData)
	}
	if _, ok := prober.videoTracks[trackID]; ok {
		return
	}
	prober.videoTracks[trackID] = stream
	prober.videoOrder = append(prober.videoOrder, trackID)
	prober.GotVideo = true
	prober.lastNewTrackPush = prober.PushedCount
}

func (prober *Prober) registerAudioTrack(trackID uint8, stream av.CodecData) {
	if prober.audioTracks == nil {
		prober.audioTracks = make(map[uint8]av.CodecData)
	}
	if _, ok := prober.audioTracks[trackID]; ok {
		return
	}
	prober.audioTracks[trackID] = stream
	prober.audioOrder = append(prober.audioOrder, trackID)
	prober.GotAudio = true
	prober.lastNewTrackPush = prober.PushedCount
}

func (prober *Prober) CacheTag(_tag flvio.Tag, timestamp int64) {
	// The tag is kept as-is and converted lazily in PopPacket(): the final
	// stream index of a track is only known once probing completed.
	prober.cachedTags = append(prober.cachedTags, cachedTag{tag: _tag, ts: timestamp})
}

func (prober *Prober) PushTag(tag flvio.Tag, timestamp int64) (err error) {
	prober.PushedCount++

	if prober.MaxProbePacketCount <= 0 {
		prober.MaxProbePacketCount = MaxProbePacketCount
	}

	if prober.PushedCount > prober.MaxProbePacketCount {
		err = fmt.Errorf("flv: max probe packet count reached")
		return
	}

	switch tag.Type {
	case flvio.TAG_VIDEO:
		if tag.IsExHeader {
			if tag.FourCC == flvio.FOURCC_HEVC {
				if tag.PacketType == flvio.PKTTYPE_SEQUENCE_START {
					var stream hevcparser.CodecData
					if stream, err = hevcparser.NewCodecDataFromHEVCDecoderConfRecord(tag.Data); err != nil {
						err = fmt.Errorf("flv: hevc seqhdr invalid: %s", err.Error())
						return
					}
					prober.registerVideoTrack(tag.TrackID, stream)
				} else if tag.PacketType == flvio.PKTTYPE_CODED_FRAMES || tag.PacketType == flvio.PKTTYPE_CODED_FRAMESX {
					prober.CacheTag(tag, timestamp)
				}
			} else if tag.FourCC == flvio.FOURCC_VP9 {
				if tag.PacketType == flvio.PKTTYPE_SEQUENCE_START {
					var stream vp9parser.CodecData
					if stream, err = vp9parser.NewCodecDataFromVPDecoderConfRecord(tag.Data); err != nil {
						err = fmt.Errorf("flv: vp9 seqhdr invalid: %s", err.Error())
						return
					}
					prober.registerVideoTrack(tag.TrackID, stream)
				} else if tag.PacketType == flvio.PKTTYPE_CODED_FRAMES || tag.PacketType == flvio.PKTTYPE_CODED_FRAMESX {
					prober.CacheTag(tag, timestamp)
				}
			} else if tag.FourCC == flvio.FOURCC_AV1 {
				if tag.PacketType == flvio.PKTTYPE_SEQUENCE_START || tag.PacketType == flvio.PKTTYPE_MPEG2TS_SEQUENCE_START {
					var stream av1parser.CodecData

					if tag.PacketType == flvio.PKTTYPE_SEQUENCE_START {
						if stream, err = av1parser.NewCodecDataFromAV1DecoderConfRecord(tag.Data); err != nil {
							err = fmt.Errorf("flv: av1 seqhdr invalid: %s", err.Error())
							return
						}
					} else {
						if stream, err = av1parser.NewCodecDataFromAV1VideoDescriptor(tag.Data); err != nil {
							err = fmt.Errorf("flv: av1 video descriptor invalid: %s", err.Error())
							return
						}
					}
					prober.registerVideoTrack(tag.TrackID, stream)
				} else if tag.FourCC == flvio.FOURCC_VVC {
					if tag.PacketType == flvio.PKTTYPE_SEQUENCE_START {
						var stream vvcparser.CodecData
						if stream, err = vvcparser.NewCodecDataFromVVCDecoderConfRecord(tag.Data); err != nil {
							err = fmt.Errorf("flv: vvc seqhdr invalid: %s", err.Error())
							return
						}
						prober.registerVideoTrack(tag.TrackID, stream)
					} else if tag.PacketType == flvio.PKTTYPE_CODED_FRAMES || tag.PacketType == flvio.PKTTYPE_CODED_FRAMESX {
						prober.CacheTag(tag, timestamp)
					}
				} else if tag.PacketType == flvio.PKTTYPE_CODED_FRAMES || tag.PacketType == flvio.PKTTYPE_CODED_FRAMESX {
					prober.CacheTag(tag, timestamp)
				}
			} else if tag.FourCC == flvio.FOURCC_AVC1 {
				if tag.PacketType == flvio.PKTTYPE_SEQUENCE_START {
					var stream h264parser.CodecData
					if stream, err = h264parser.NewCodecDataFromAVCDecoderConfRecord(tag.Data); err != nil {
						err = fmt.Errorf("flv: h264 seqhdr invalid: %s", err.Error())
						return
					}
					prober.registerVideoTrack(tag.TrackID, stream)
				} else if tag.PacketType == flvio.PKTTYPE_CODED_FRAMES || tag.PacketType == flvio.PKTTYPE_CODED_FRAMESX {
					prober.CacheTag(tag, timestamp)
				}
			}
		} else {
			switch tag.AVCPacketType {
			case flvio.AVC_SEQHDR:
				var stream h264parser.CodecData
				if stream, err = h264parser.NewCodecDataFromAVCDecoderConfRecord(tag.Data); err != nil {
					err = fmt.Errorf("flv: h264 seqhdr invalid: %s", err.Error())
					return
				}
				prober.registerVideoTrack(tag.TrackID, stream)

			case flvio.AVC_NALU:
				prober.CacheTag(tag, timestamp)
			}
		}

	case flvio.TAG_AUDIO:
		if tag.IsExHeader {
			// Enhanced RTMP (E-RTMP v2) audio, possibly multi-track
			switch tag.PacketType {
			case flvio.PKTTYPE_SEQUENCE_START:
				if tag.FourCC != flvio.FOURCC_MP4A {
					err = fmt.Errorf("flv: unsupported ex audio fourcc '%s'", string(tag.FourCC[:]))
					return
				}
				var stream aacparser.CodecData
				if stream, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(tag.Data); err != nil {
					err = fmt.Errorf("flv: aac seqhdr invalid")
					return
				}
				prober.registerAudioTrack(tag.TrackID, stream)

			case flvio.PKTTYPE_CODED_FRAMES, flvio.PKTTYPE_CODED_FRAMESX:
				prober.CacheTag(tag, timestamp)
			}

			return
		}

		switch tag.SoundFormat {
		case flvio.SOUND_AAC:
			switch tag.AACPacketType {
			case flvio.AAC_SEQHDR:
				var stream aacparser.CodecData
				if stream, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(tag.Data); err != nil {
					err = fmt.Errorf("flv: aac seqhdr invalid")
					return
				}
				prober.registerAudioTrack(tag.TrackID, stream)

			case flvio.AAC_RAW:
				prober.CacheTag(tag, timestamp)
			}

		case flvio.SOUND_SPEEX:
			if !prober.GotAudio {
				stream := codec.NewSpeexCodecData(16000, tag.ChannelLayout())
				prober.registerAudioTrack(tag.TrackID, stream)
			}
			prober.CacheTag(tag, timestamp)

		case flvio.SOUND_NELLYMOSER:
			if !prober.GotAudio {
				stream := fake.CodecData{
					CodecType_:     av.NELLYMOSER,
					SampleRate_:    16000,
					SampleFormat_:  av.S16,
					ChannelLayout_: tag.ChannelLayout(),
				}
				prober.registerAudioTrack(tag.TrackID, stream)
			}
			prober.CacheTag(tag, timestamp)
		}
	}

	return
}

// freeze builds the final, canonical stream layout: video tracks first, then
// audio tracks, each ordered by ascending wire track ID. This makes the
// stream indexes (and thereby FFmpeg's 0:a:N ordinals on the loopback
// round-trip) deterministic across reconnects. It is idempotent.
func (prober *Prober) freeze() {
	if prober.frozen {
		return
	}
	prober.frozen = true

	videoIDs := append([]uint8(nil), prober.videoOrder...)
	audioIDs := append([]uint8(nil), prober.audioOrder...)
	sort.Slice(videoIDs, func(i, j int) bool { return videoIDs[i] < videoIDs[j] })
	sort.Slice(audioIDs, func(i, j int) bool { return audioIDs[i] < audioIDs[j] })

	prober.Streams = nil
	prober.videoTrackIdx = make(map[uint8]int8, len(videoIDs))
	prober.audioTrackIdx = make(map[uint8]int8, len(audioIDs))
	prober.AudioStreamIdxs = nil

	for _, id := range videoIDs {
		prober.videoTrackIdx[id] = int8(len(prober.Streams))
		prober.Streams = append(prober.Streams, prober.videoTracks[id])
	}

	for _, id := range audioIDs {
		prober.audioTrackIdx[id] = int8(len(prober.Streams))
		prober.AudioStreamIdxs = append(prober.AudioStreamIdxs, len(prober.Streams))
		prober.Streams = append(prober.Streams, prober.audioTracks[id])
	}

	if len(videoIDs) > 0 {
		prober.VideoStreamIdx = int(prober.videoTrackIdx[videoIDs[0]])
	}

	if len(prober.AudioStreamIdxs) > 0 {
		prober.AudioStreamIdx = prober.AudioStreamIdxs[0]
	}
}

// Probed reports whether probing is complete. Once the declared streams are
// present, a quiet window of pushes without newly discovered tracks must pass
// before probing completes: additional audio tracks (e.g. an E-RTMP "VOD
// track") announce themselves right after the first audio track, and track
// registration must stay open for them. Legacy-only streams pay the same tiny
// window, which is the only way to know no second track is coming.
func (prober *Prober) Probed() (ok bool) {
	if prober.MaxProbePacketCount <= 0 {
		prober.MaxProbePacketCount = MaxProbePacketCount
	}

	if prober.frozen {
		return true
	}

	if prober.HasAudio || prober.HasVideo {
		if prober.HasAudio == prober.GotAudio && prober.HasVideo == prober.GotVideo {
			if prober.PushedCount-prober.lastNewTrackPush >= ProbeQuietWindow {
				prober.freeze()
				return true
			}
			return false
		}
	}

	if prober.PushedCount >= prober.MaxProbePacketCount {
		// Hard stop: complete probing with whatever was discovered.
		prober.freeze()
		return true
	}

	return false
}

// Finish ends probing immediately with whatever tracks were discovered so
// far. It is meant for streams that end (or break off) before the probe quiet
// window could close.
func (prober *Prober) Finish() {
	prober.freeze()
}

func (prober *Prober) TagToPacket(tag flvio.Tag, timestamp int64) (pkt av.Packet, ok bool) {
	prober.freeze()
	switch tag.Type {
	case flvio.TAG_VIDEO:
		idx, known := prober.videoTrackIdx[tag.TrackID]
		if !known {
			prober.DroppedUnknownTrack++
			return
		}
		pkt.Idx = idx
		switch tag.PacketType {
		case flvio.PKTTYPE_CODED_FRAMES, flvio.PKTTYPE_CODED_FRAMESX:
			ok = true
			pkt.Data = tag.Data
			pkt.CompositionTime = flvio.TsToTime(tag.CompositionTime)
			pkt.IsKeyFrame = tag.FrameType == flvio.FRAME_KEY
		}

	case flvio.TAG_AUDIO:
		idx, known := prober.audioTrackIdx[tag.TrackID]
		if !known {
			prober.DroppedUnknownTrack++
			return
		}
		pkt.Idx = idx
		if tag.IsExHeader {
			switch tag.PacketType {
			case flvio.PKTTYPE_CODED_FRAMES, flvio.PKTTYPE_CODED_FRAMESX:
				ok = true
				pkt.Data = tag.Data
				pkt.IsKeyFrame = true
			}
		} else {
			switch tag.SoundFormat {
			case flvio.SOUND_AAC:
				if tag.AACPacketType == flvio.AAC_RAW {
					ok = true
					pkt.Data = tag.Data
				}

			case flvio.SOUND_SPEEX, flvio.SOUND_NELLYMOSER:
				ok = true
				pkt.Data = tag.Data
				pkt.IsKeyFrame = true
			}
		}
	}

	pkt.Time = flvio.TsToTime(timestamp)
	return
}

func (prober *Prober) Empty() bool {
	return len(prober.cachedTags) == 0
}

func (prober *Prober) PopPacket() (pkt av.Packet, ok bool) {
	for len(prober.cachedTags) > 0 {
		cached := prober.cachedTags[0]
		prober.cachedTags = prober.cachedTags[1:]
		if pkt, ok = prober.TagToPacket(cached.tag, cached.ts); ok {
			return
		}
	}
	return
}

// TrackIDsForStreams assigns each stream its egress wire track ID: per media
// type, numbered in stream order. The audio ordinal of a stream (its position
// among the audio streams) thereby equals its E-RTMP track ID on the wire and
// its FFmpeg 0:a:N ordinal on the loopback round-trip.
func TrackIDsForStreams(streams []av.CodecData) []uint8 {
	trackIDs := make([]uint8, len(streams))

	var videoCount, audioCount uint8

	for i, stream := range streams {
		switch {
		case stream.Type().IsVideo():
			trackIDs[i] = videoCount
			videoCount++
		case stream.Type().IsAudio():
			trackIDs[i] = audioCount
			audioCount++
		}
	}

	return trackIDs
}

// CodecDataToTag builds the codec config tag for a stream. Track 0 audio and
// all video are written with legacy FLV framing for compatibility; audio
// tracks >= 1 are written as Enhanced RTMP (E-RTMP v2) multitrack messages
// (AAC/mp4a only).
func CodecDataToTag(stream av.CodecData, track uint8) (_tag flvio.Tag, ok bool, err error) {
	switch stream.Type() {
	case av.H264:
		if track != 0 {
			err = fmt.Errorf("flv: multi-track video is not supported")
			return
		}
		h264 := stream.(h264parser.CodecData)
		tag := flvio.Tag{
			Type:          flvio.TAG_VIDEO,
			AVCPacketType: flvio.AVC_SEQHDR,
			CodecID:       flvio.VIDEO_H264,
			Data:          h264.AVCDecoderConfRecordBytes(),
			FrameType:     flvio.FRAME_KEY,
		}
		//fmt.Printf("set H264 sequence start:\n%v\n", hex.Dump(tag.Data))
		ok = true
		_tag = tag

	case av.HEVC:
		hevc := stream.(hevcparser.CodecData)
		tag := flvio.Tag{
			Type:       flvio.TAG_VIDEO,
			IsExHeader: true,
			PacketType: flvio.PKTTYPE_SEQUENCE_START,
			FourCC:     flvio.FOURCC_HEVC,
			Data:       hevc.HEVCDecoderConfRecordBytes(),
			FrameType:  flvio.FRAME_KEY,
		}
		//fmt.Printf("set HEVC sequence start:\n%v\n", hex.Dump(tag.Data))
		ok = true
		_tag = tag

	case av.VP9:
		vp9 := stream.(vp9parser.CodecData)
		tag := flvio.Tag{
			Type:       flvio.TAG_VIDEO,
			IsExHeader: true,
			PacketType: flvio.PKTTYPE_SEQUENCE_START,
			FourCC:     flvio.FOURCC_VP9,
			Data:       vp9.VPDecoderConfRecordBytes(),
			FrameType:  flvio.FRAME_KEY,
		}
		//fmt.Printf("set VP9 sequence start:\n%v\n", hex.Dump(tag.Data))
		ok = true
		_tag = tag

	case av.AV1:
		av1 := stream.(av1parser.CodecData)
		tag := flvio.Tag{
			Type:       flvio.TAG_VIDEO,
			IsExHeader: true,
			PacketType: flvio.PKTTYPE_SEQUENCE_START,
			FourCC:     flvio.FOURCC_AV1,
			Data:       av1.AV1DecoderConfRecordBytes(),
			FrameType:  flvio.FRAME_KEY,
		}

		if av1.IsMpeg2TS {
			tag.PacketType = flvio.PKTTYPE_MPEG2TS_SEQUENCE_START
			tag.Data = av1.AV1VideoDescriptorBytes()
		}

		//fmt.Printf("set AV1 sequence start:\n%v\n", hex.Dump(tag.Data))
		ok = true
		_tag = tag

	case av.VVC:
		vvc := stream.(vvcparser.CodecData)
		tag := flvio.Tag{
			Type:       flvio.TAG_VIDEO,
			IsExHeader: true,
			PacketType: flvio.PKTTYPE_SEQUENCE_START,
			FourCC:     flvio.FOURCC_VVC,
			Data:       vvc.VVCDecoderConfRecordBytes(),
			FrameType:  flvio.FRAME_KEY,
		}

		//fmt.Printf("set AV1 sequence start:\n%v\n", hex.Dump(tag.Data))
		ok = true
		_tag = tag

	case av.AAC:
		aac := stream.(aacparser.CodecData)
		if track == 0 {
			tag := flvio.Tag{
				Type:          flvio.TAG_AUDIO,
				SoundFormat:   flvio.SOUND_AAC,
				SoundRate:     flvio.SOUND_44Khz,
				AACPacketType: flvio.AAC_SEQHDR,
				Data:          aac.MPEG4AudioConfigBytes(),
			}
			switch aac.SampleFormat().BytesPerSample() {
			case 1:
				tag.SoundSize = flvio.SOUND_8BIT
			default:
				tag.SoundSize = flvio.SOUND_16BIT
			}
			switch aac.ChannelLayout().Count() {
			case 1:
				tag.SoundType = flvio.SOUND_MONO
			case 2:
				tag.SoundType = flvio.SOUND_STEREO
			}
			ok = true
			_tag = tag
		} else {
			tag := flvio.Tag{
				Type:           flvio.TAG_AUDIO,
				SoundFormat:    flvio.SOUND_EXHEADER,
				IsExHeader:     true,
				IsMultitrack:   true,
				MultitrackType: flvio.MULTITRACK_ONETRACK,
				PacketType:     flvio.PKTTYPE_SEQUENCE_START,
				FourCC:         flvio.FOURCC_MP4A,
				TrackID:        track,
				Data:           aac.MPEG4AudioConfigBytes(),
			}
			ok = true
			_tag = tag
		}

	case av.NELLYMOSER:
	case av.SPEEX:
		if track != 0 {
			err = fmt.Errorf("flv: multi-track audio requires AAC")
			return
		}

	default:
		err = fmt.Errorf("flv: unspported codecType=%v", stream.Type())
		return
	}
	return
}

// PacketToTag builds the media tag for a packet. Audio track 0 keeps legacy
// FLV framing; audio tracks >= 1 are written as Enhanced RTMP (E-RTMP v2)
// multitrack messages (AAC/mp4a only). ok is false for streams that cannot be
// represented (they must be skipped by the muxer).
func PacketToTag(pkt av.Packet, stream av.CodecData, track uint8) (tag flvio.Tag, timestamp int64, ok bool) {
	switch stream.Type() {
	case av.H264:
		if track != 0 {
			return
		}
		tag = flvio.Tag{
			Type:            flvio.TAG_VIDEO,
			AVCPacketType:   flvio.AVC_NALU,
			CodecID:         flvio.VIDEO_H264,
			Data:            pkt.Data,
			CompositionTime: flvio.TimeToTs(pkt.CompositionTime),
		}
		if pkt.IsKeyFrame {
			tag.FrameType = flvio.FRAME_KEY
		} else {
			tag.FrameType = flvio.FRAME_INTER
		}
		ok = true

	case av.HEVC:
		if track != 0 {
			return
		}
		tag = flvio.Tag{
			Type:            flvio.TAG_VIDEO,
			IsExHeader:      true,
			PacketType:      flvio.PKTTYPE_CODED_FRAMES,
			CompositionTime: flvio.TimeToTs(pkt.CompositionTime),
			FourCC:          flvio.FOURCC_HEVC,
			Data:            pkt.Data,
		}

		if pkt.CompositionTime == 0 {
			tag.PacketType = flvio.PKTTYPE_CODED_FRAMESX
		}

		if pkt.IsKeyFrame {
			tag.FrameType = flvio.FRAME_KEY
		} else {
			tag.FrameType = flvio.FRAME_INTER
		}
		ok = true

	case av.VP9:
		if track != 0 {
			return
		}
		tag = flvio.Tag{
			Type:            flvio.TAG_VIDEO,
			IsExHeader:      true,
			PacketType:      flvio.PKTTYPE_CODED_FRAMES,
			CompositionTime: flvio.TimeToTs(pkt.CompositionTime),
			FourCC:          flvio.FOURCC_VP9,
			Data:            pkt.Data,
		}

		if pkt.IsKeyFrame {
			tag.FrameType = flvio.FRAME_KEY
		} else {
			tag.FrameType = flvio.FRAME_INTER
		}
		ok = true

	case av.AV1:
		if track != 0 {
			return
		}
		tag = flvio.Tag{
			Type:            flvio.TAG_VIDEO,
			IsExHeader:      true,
			PacketType:      flvio.PKTTYPE_CODED_FRAMES,
			CompositionTime: flvio.TimeToTs(pkt.CompositionTime),
			FourCC:          flvio.FOURCC_AV1,
			Data:            pkt.Data,
		}

		if pkt.IsKeyFrame {
			tag.FrameType = flvio.FRAME_KEY
		} else {
			tag.FrameType = flvio.FRAME_INTER
		}
		ok = true

	case av.VVC:
		tag = flvio.Tag{
			Type:            flvio.TAG_VIDEO,
			IsExHeader:      true,
			PacketType:      flvio.PKTTYPE_CODED_FRAMES,
			CompositionTime: flvio.TimeToTs(pkt.CompositionTime),
			FourCC:          flvio.FOURCC_VVC,
			Data:            pkt.Data,
		}

		if pkt.IsKeyFrame {
			tag.FrameType = flvio.FRAME_KEY
		} else {
			tag.FrameType = flvio.FRAME_INTER
		}

	case av.AAC:
		if track == 0 {
			tag = flvio.Tag{
				Type:          flvio.TAG_AUDIO,
				SoundFormat:   flvio.SOUND_AAC,
				SoundRate:     flvio.SOUND_44Khz,
				AACPacketType: flvio.AAC_RAW,
				Data:          pkt.Data,
			}
			astream := stream.(av.AudioCodecData)
			switch astream.SampleFormat().BytesPerSample() {
			case 1:
				tag.SoundSize = flvio.SOUND_8BIT
			default:
				tag.SoundSize = flvio.SOUND_16BIT
			}
			switch astream.ChannelLayout().Count() {
			case 1:
				tag.SoundType = flvio.SOUND_MONO
			case 2:
				tag.SoundType = flvio.SOUND_STEREO
			}
		} else {
			tag = flvio.Tag{
				Type:           flvio.TAG_AUDIO,
				SoundFormat:    flvio.SOUND_EXHEADER,
				IsExHeader:     true,
				IsMultitrack:   true,
				MultitrackType: flvio.MULTITRACK_ONETRACK,
				PacketType:     flvio.PKTTYPE_CODED_FRAMES,
				FourCC:         flvio.FOURCC_MP4A,
				TrackID:        track,
				Data:           pkt.Data,
			}
		}
		ok = true

	case av.SPEEX:
		if track != 0 {
			return
		}
		tag = flvio.Tag{
			Type:        flvio.TAG_AUDIO,
			SoundFormat: flvio.SOUND_SPEEX,
			Data:        pkt.Data,
		}
		ok = true

	case av.NELLYMOSER:
		if track != 0 {
			return
		}
		tag = flvio.Tag{
			Type:        flvio.TAG_AUDIO,
			SoundFormat: flvio.SOUND_NELLYMOSER,
			Data:        pkt.Data,
		}
		ok = true
	}

	timestamp = flvio.TimeToTs(pkt.Time)
	return
}

type Muxer struct {
	bufw     writeFlusher
	b        []byte
	streams  []av.CodecData
	trackIDs []uint8
}

type writeFlusher interface {
	io.Writer
	Flush() error
}

func NewMuxerWriteFlusher(w writeFlusher) *Muxer {
	return &Muxer{
		bufw: w,
		b:    make([]byte, 256),
	}
}

func NewMuxer(w io.Writer) *Muxer {
	return NewMuxerWriteFlusher(bufio.NewWriterSize(w, pio.RecommendBufioSize))
}

var CodecTypes = []av.CodecType{av.H264, av.HEVC, av.VP9, av.AV1, av.VVC, av.AAC, av.SPEEX}

func (muxer *Muxer) WriteHeader(streams []av.CodecData) (err error) {
	var flags uint8
	for _, stream := range streams {
		if stream.Type().IsVideo() {
			flags |= flvio.FILE_HAS_VIDEO
		} else if stream.Type().IsAudio() {
			flags |= flvio.FILE_HAS_AUDIO
		}
	}

	n := flvio.FillFileHeader(muxer.b, flags)
	if _, err = muxer.bufw.Write(muxer.b[:n]); err != nil {
		return
	}

	trackIDs := TrackIDsForStreams(streams)

	for i, stream := range streams {
		var tag flvio.Tag
		var ok bool
		if tag, ok, err = CodecDataToTag(stream, trackIDs[i]); err != nil {
			return
		}
		if ok {
			if err = flvio.WriteTag(muxer.bufw, tag, 0, muxer.b); err != nil {
				return
			}
		}
	}

	muxer.streams = streams
	muxer.trackIDs = trackIDs
	return
}

func (muxer *Muxer) WritePacket(pkt av.Packet) (err error) {
	stream := muxer.streams[pkt.Idx]
	tag, timestamp, ok := PacketToTag(pkt, stream, muxer.trackIDs[pkt.Idx])
	if !ok {
		return
	}

	if err = flvio.WriteTag(muxer.bufw, tag, timestamp, muxer.b); err != nil {
		return
	}
	return
}

func (muxer *Muxer) WriteTrailer() (err error) {
	if err = muxer.bufw.Flush(); err != nil {
		return
	}
	return
}

type Demuxer struct {
	prober *Prober
	bufr   *bufio.Reader
	b      []byte
	stage  int
}

func NewDemuxer(r io.Reader) *Demuxer {
	return &Demuxer{
		bufr:   bufio.NewReaderSize(r, pio.RecommendBufioSize),
		prober: &Prober{},
		b:      make([]byte, 256),
	}
}

func (demuxer *Demuxer) prepare() (err error) {
	for demuxer.stage < 2 {
		switch demuxer.stage {
		case 0:
			if _, err = io.ReadFull(demuxer.bufr, demuxer.b[:flvio.FileHeaderLength]); err != nil {
				return
			}
			var flags uint8
			var skip int
			if flags, skip, err = flvio.ParseFileHeader(demuxer.b); err != nil {
				return
			}
			if _, err = demuxer.bufr.Discard(skip); err != nil {
				return
			}
			if flags&flvio.FILE_HAS_AUDIO != 0 {
				demuxer.prober.HasAudio = true
			}
			if flags&flvio.FILE_HAS_VIDEO != 0 {
				demuxer.prober.HasVideo = true
			}
			demuxer.stage++

		case 1:
			for !demuxer.prober.Probed() {
				var tag flvio.Tag
				var timestamp int64
				if tag, timestamp, err = flvio.ReadTag(demuxer.bufr, demuxer.b); err != nil {
					if err == io.EOF && (demuxer.prober.GotAudio || demuxer.prober.GotVideo) {
						// The stream ended before the probe quiet window could
						// close: accept whatever was discovered.
						demuxer.prober.Finish()
						err = nil
						break
					}
					return
				}
				if err = demuxer.prober.PushTag(tag, timestamp); err != nil {
					return
				}
			}
			demuxer.stage++
		}
	}
	return
}

func (demuxer *Demuxer) Streams() (streams []av.CodecData, err error) {
	if err = demuxer.prepare(); err != nil {
		return
	}
	streams = demuxer.prober.Streams
	return
}

func (demuxer *Demuxer) ReadPacket() (pkt av.Packet, err error) {
	if err = demuxer.prepare(); err != nil {
		return
	}

	for !demuxer.prober.Empty() {
		var ok bool
		if pkt, ok = demuxer.prober.PopPacket(); ok {
			return
		}
	}

	for {
		var tag flvio.Tag
		var timestamp int64
		if tag, timestamp, err = flvio.ReadTag(demuxer.bufr, demuxer.b); err != nil {
			return
		}

		var ok bool
		if pkt, ok = demuxer.prober.TagToPacket(tag, timestamp); ok {
			return
		}
	}
}

func Handler(h *avutil.RegisterHandler) {
	h.Probe = func(b []byte) bool {
		return b[0] == 'F' && b[1] == 'L' && b[2] == 'V'
	}

	h.Ext = ".flv"

	h.ReaderDemuxer = func(r io.Reader) av.Demuxer {
		return NewDemuxer(r)
	}

	h.WriterMuxer = func(w io.Writer) av.Muxer {
		return NewMuxer(w)
	}

	h.CodecTypes = CodecTypes
}
