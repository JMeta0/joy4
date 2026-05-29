package vvcparser

import (
	"github.com/datarhei/joy4/av"
)

type CodecData struct {
	Record []byte
}

func (codec CodecData) Type() av.CodecType {
	return av.VVC
}

func (codec CodecData) VVCDecoderConfRecordBytes() []byte {
	return codec.Record
}

func (codec CodecData) VVCVideoDescriptorBytes() []byte {
	return codec.Record
}

func (codec CodecData) Width() int {
	return 0
}

func (codec CodecData) Height() int {
	return 0
}

func NewCodecDataFromVVCDecoderConfRecord(record []byte) (data CodecData, err error) {
	data.Record = record

	return
}

func NewCodecDataFromVVCVideoDescriptor(record []byte) (data CodecData, err error) {
	data.Record = record

	return
}
