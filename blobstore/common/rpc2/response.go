// Copyright 2024 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package rpc2

import (
	"bytes"
	"context"
	"io"
	"sync"

	"github.com/cubefs/cubefs/blobstore/common/rpc2/transport"
)

// server side response
type ResponseWriter interface {
	SetContentLength(int64)
	Header() *Header
	Trailer() *FixedHeader

	// WriteHeader object in Header's Parameter
	WriteHeader(status int, obj Marshaler) error
	// WriteOK object in body
	WriteOK(obj Marshaler) error
	// SetError fill error's reason to response header
	SetError(err error)
	Flush() error
	// io.Writer
	io.ReaderFrom

	AfterBody(func() error)
}

// client side response
type Response struct {
	ResponseHeader

	Body Body

	Request *Request
}

var _ ResponseWriter = &response{}

func (resp *Response) ParseResult(ret Unmarshaler) error {
	if ret == nil {
		return nil
	}
	if len(resp.Parameter) > 0 {
		return ret.Unmarshal(resp.Parameter[:])
	}
	if resp.ContentLength == 0 {
		return ret.Unmarshal(nil)
	}
	_, err := resp.Body.WriteTo(LimitWriter(Codec2Writer(ret, int(resp.ContentLength)), resp.ContentLength))
	return err
}

type response struct {
	hdr ResponseHeader

	ctx        context.Context
	conn       *transport.Stream
	connBroken bool

	hasWroteHeader bool
	hasWroteBody   bool

	bodyEncoder *edBody

	remain    int // body remain
	toWrite   int
	toList    []io.Reader
	afterBody func() error
}

func (resp *response) SetContentLength(l int64) {
	resp.hdr.ContentLength = l
	resp.remain = int(l)
	if resp.bodyEncoder != nil {
		resp.bodyEncoder.remain = int(l)
	}
}

func (resp *response) Header() *Header {
	return &resp.hdr.Header
}

func (resp *response) Trailer() *FixedHeader {
	return &resp.hdr.Trailer
}

func (resp *response) SetError(err error) {
	_, reason, detail := DetectError(err)
	resp.hdr.Reason = reason
	resp.hdr.Error = detail.Error()
}

func (resp *response) WriteOK(obj Marshaler) error {
	if resp.hasWroteHeader {
		return nil
	}
	if obj == nil {
		obj = NoParameter
	}
	size := int64(obj.Size())
	resp.SetContentLength(int64(size))
	_, err := resp.ReadFrom(Codec2Reader(obj))
	return err
}

func (resp *response) WriteHeader(status int, obj Marshaler) error {
	if resp.hasWroteHeader {
		return nil
	}
	resp.hdr.Status = int32(status)
	resp.hasWroteHeader = true
	resp.hdr.Header.SetStable()
	resp.hdr.Trailer.SetStable()

	if obj == nil {
		obj = NoParameter
	}
	b, err := obj.Marshal()
	if err != nil {
		return err
	}
	resp.hdr.Parameter = b

	var cell headerCell
	cell.Set(resp.hdr.Size())
	resp.toWrite += _headerCell + resp.hdr.Size()
	resp.toList = append(resp.toList, codec2CellReader(cell, &resp.hdr))
	return nil
}

func (resp *response) Write(p []byte) (int, error) {
	if !resp.hasWroteHeader {
		if err := resp.WriteHeader(200, NoParameter); err != nil {
			return 0, err
		}
	}
	if resp.remain < len(p) {
		p = p[:resp.remain]
	}
	if resp.remain != len(p) {
		return 0, io.ErrShortWrite
	}
	if resp.hasWroteBody {
		return 0, nil
	}
	resp.hasWroteBody = true

	r, toWrite := resp.encodeBody(bytes.NewReader(p))
	resp.toWrite += toWrite + resp.hdr.Trailer.AllSize()
	resp.toList = append(resp.toList, r, &trailerReader{
		Fn:      resp.afterBody,
		Trailer: &resp.hdr.Trailer,
	})
	resp.remain = 0
	if err := resp.Flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (resp *response) ReadFrom(r io.Reader) (n int64, err error) {
	if !resp.hasWroteHeader {
		if err := resp.WriteHeader(200, NoParameter); err != nil {
			return 0, err
		}
	}
	if resp.hasWroteBody {
		return 0, nil
	}
	resp.hasWroteBody = true

	remain := resp.remain
	r, toWrite := resp.encodeBody(io.LimitReader(r, int64(remain)))
	resp.toWrite += toWrite + resp.hdr.Trailer.AllSize()
	resp.toList = append(resp.toList, r, &trailerReader{
		Fn:      resp.afterBody,
		Trailer: &resp.hdr.Trailer,
	})
	resp.remain = 0
	if err := resp.Flush(); err != nil {
		return 0, err
	}
	return int64(remain), nil
}

func (resp *response) Flush() error {
	if len(resp.toList) == 0 {
		return nil
	}
	if resp.connBroken {
		return io.ErrClosedPipe
	}
	_, err := resp.conn.SizedWrite(resp.ctx, io.MultiReader(resp.toList...), resp.toWrite)
	if err != nil {
		resp.connBroken = true
		return err
	}
	resp.toWrite = 0
	resp.toList = resp.toList[:0]
	return nil
}

func (resp *response) AfterBody(fn func() error) {
	afterBody := resp.afterBody
	resp.afterBody = func() error {
		if err := fn(); err != nil {
			return err
		}
		if afterBody != nil {
			return afterBody()
		}
		return nil
	}
}

func (resp *response) options(req *Request) {
	if req.checksum != (ChecksumBlock{}) && req.checksum.Direction.IsDownload() {
		resp.bodyEncoder = newEdBody(req.checksum, nil, 0, true)
	}
}

func (resp *response) encodeBody(r io.Reader) (io.Reader, int) {
	if resp.bodyEncoder == nil {
		return r, resp.remain
	}
	resp.bodyEncoder.Body = clientNopBody(io.NopCloser(r))
	return resp.bodyEncoder, int(resp.bodyEncoder.block.EncodeSize(int64(resp.remain)))
}

func (resp *response) reuse() {
	putResponse(resp)
}

var poolResponse = sync.Pool{
	New: func() any {
		return &response{
			hdr: ResponseHeader{
				Version: Version,
				Magic:   Magic,
			},
		}
	},
}

func getResponse() *response {
	return poolResponse.Get().(*response)
}

func putResponse(resp *response) {
	resp.hdr.Status = 0
	resp.hdr.Reason = ""
	resp.hdr.Error = ""
	resp.hdr.ContentLength = 0
	resp.hdr.Header.Renew()
	resp.hdr.Trailer.Renew()
	resp.hdr.Parameter = resp.hdr.Parameter[:0]

	resp.ctx = nil
	resp.conn = nil
	resp.connBroken = false

	resp.hasWroteHeader = false
	resp.hasWroteBody = false
	resp.bodyEncoder = nil

	resp.remain = 0
	resp.toWrite = 0
	resp.toList = resp.toList[:0]
	resp.afterBody = nil

	poolResponse.Put(resp) // nolint: staticcheck
}
