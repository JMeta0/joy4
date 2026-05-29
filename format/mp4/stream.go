package mp4

import (
	"github.com/datarhei/joy4/av"
	"github.com/datarhei/joy4/format/mp4/mp4io"
)

type Stream struct {
	av.CodecData

	trackAtom *mp4io.Track
	idx       int

	lastpkt *av.Packet

	timeScale int64
	duration  int64

	muxer   *Muxer
	demuxer *Demuxer

	sample      *mp4io.SampleTable
	sampleIndex int

	sampleOffsetInChunk int64
	syncSampleIndex     int

	dts                    int64
	sttsEntryIndex         int
	sampleIndexInSttsEntry int

	cttsEntryIndex         int
	sampleIndexInCttsEntry int

	chunkGroupIndex    int
	chunkIndex         int
	sampleIndexInChunk int

	sttsEntry *mp4io.TimeToSampleEntry
	cttsEntry *mp4io.CompositionOffsetEntry
}

func timeToTs(tm int64, timeScale int64) int64 {
	return tm * timeScale
}

func tsToTime(ts int64, timeScale int64) int64 {
	return ts / timeScale
}

func (self *Stream) timeToTs(tm int64) int64 {
	return tm * self.timeScale
}

func (self *Stream) tsToTime(ts int64) int64 {
	return ts / self.timeScale
}
