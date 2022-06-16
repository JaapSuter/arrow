// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/apache/arrow/go/v9/arrow"
	"github.com/apache/arrow/go/v9/arrow/array"
	"github.com/apache/arrow/go/v9/arrow/bitutil"
	"github.com/apache/arrow/go/v9/arrow/internal/dictutils"
	"github.com/apache/arrow/go/v9/arrow/internal/flatbuf"
	"github.com/apache/arrow/go/v9/arrow/memory"
)

type swriter struct {
	w   io.Writer
	pos int64
}

func (w *swriter) Start() error { return nil }
func (w *swriter) Close() error {
	_, err := w.Write(kEOS[:])
	return err
}

func (w *swriter) WritePayload(p Payload) error {
	_, err := writeIPCPayload(w, p)
	if err != nil {
		return err
	}
	return nil
}

func (w *swriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.pos += int64(n)
	return n, err
}

func hasNestedDict(data arrow.ArrayData) bool {
	if data.DataType().ID() == arrow.DICTIONARY {
		return true
	}
	for _, c := range data.Children() {
		if hasNestedDict(c) {
			return true
		}
	}
	return false
}

// Writer is an Arrow stream writer.
type Writer struct {
	w io.Writer

	mem memory.Allocator
	pw  PayloadWriter

	started    bool
	schema     *arrow.Schema
	mapper     dictutils.Mapper
	codec      flatbuf.CompressionType
	compressNP int

	// map of the last written dictionaries by id
	// so we can avoid writing the same dictionary over and over
	lastWrittenDicts map[int64]arrow.Array
	emitDictDeltas   bool
}

// NewWriterWithPayloadWriter constructs a writer with the provided payload writer
// instead of the default stream payload writer. This makes the writer more
// reusable such as by the Arrow Flight writer.
func NewWriterWithPayloadWriter(pw PayloadWriter, opts ...Option) *Writer {
	cfg := newConfig(opts...)
	return &Writer{
		mem:        cfg.alloc,
		pw:         pw,
		schema:     cfg.schema,
		codec:      cfg.codec,
		compressNP: cfg.compressNP,
	}
}

// NewWriter returns a writer that writes records to the provided output stream.
func NewWriter(w io.Writer, opts ...Option) *Writer {
	cfg := newConfig(opts...)
	return &Writer{
		w:      w,
		mem:    cfg.alloc,
		pw:     &swriter{w: w},
		schema: cfg.schema,
		codec:  cfg.codec,
	}
}

func (w *Writer) Close() error {
	if !w.started {
		err := w.start()
		if err != nil {
			return err
		}
	}

	if w.pw == nil {
		return nil
	}

	err := w.pw.Close()
	if err != nil {
		return fmt.Errorf("arrow/ipc: could not close payload writer: %w", err)
	}
	w.pw = nil

	for _, d := range w.lastWrittenDicts {
		d.Release()
	}

	return nil
}

func (w *Writer) Write(rec arrow.Record) (err error) {
	defer func() {
		if pErr := recover(); pErr != nil {
			err = fmt.Errorf("arrow/ipc: unknown error while writing: %v", pErr)
		}
	}()

	if !w.started {
		err := w.start()
		if err != nil {
			return err
		}
	}

	schema := rec.Schema()
	if schema == nil || !schema.Equal(w.schema) {
		return errInconsistentSchema
	}

	const allow64b = true
	var (
		data = Payload{msg: MessageRecordBatch}
		enc  = newRecordEncoder(w.mem, 0, kMaxNestingDepth, allow64b, w.codec, w.compressNP)
	)
	defer data.Release()

	err = writeDictionaryPayloads(w.mem, rec, false, w.emitDictDeltas, &w.mapper, w.lastWrittenDicts, w.pw, enc)
	if err != nil {
		return fmt.Errorf("arrow/ipc: failure writing dictionary batches: %w", err)
	}

	enc.reset()
	if err := enc.Encode(&data, rec); err != nil {
		return fmt.Errorf("arrow/ipc: could not encode record to payload: %w", err)
	}

	return w.pw.WritePayload(data)
}

func writeDictionaryPayloads(mem memory.Allocator, batch arrow.Record, isFileFormat bool, emitDictDeltas bool, mapper *dictutils.Mapper, lastWrittenDicts map[int64]arrow.Array, pw PayloadWriter, encoder *recordEncoder) error {
	dictionaries, err := dictutils.CollectDictionaries(batch, mapper)
	if err != nil {
		return err
	}
	defer func() {
		for _, d := range dictionaries {
			d.Dict.Release()
		}
	}()

	eqopt := array.WithNaNsEqual(true)
	for _, pair := range dictionaries {
		encoder.reset()
		var (
			deltaStart int64
			enc        = dictEncoder{encoder}
		)
		lastDict, exists := lastWrittenDicts[pair.ID]
		if exists {
			if lastDict.Data() == pair.Dict.Data() {
				continue
			}
			newLen, lastLen := pair.Dict.Len(), lastDict.Len()
			if lastLen == newLen && array.ApproxEqual(lastDict, pair.Dict, eqopt) {
				// same dictionary by value
				// might cost CPU, but required for IPC file format
				continue
			}
			if isFileFormat {
				return errors.New("arrow/ipc: Dictionary replacement detected when writing IPC file format. Arrow IPC File only supports single dictionary per field")
			}

			if newLen > lastLen &&
				emitDictDeltas &&
				!hasNestedDict(pair.Dict.Data()) &&
				(array.SliceApproxEqual(lastDict, 0, int64(lastLen), pair.Dict, 0, int64(lastLen), eqopt)) {
				deltaStart = int64(lastLen)
			}
		}

		var data = Payload{msg: MessageDictionaryBatch}
		defer data.Release()

		dict := pair.Dict
		if deltaStart > 0 {
			dict = array.NewSlice(dict, deltaStart, int64(dict.Len()))
			defer dict.Release()
		}
		if err := enc.Encode(&data, pair.ID, deltaStart > 0, dict); err != nil {
			return err
		}

		if err := pw.WritePayload(data); err != nil {
			return err
		}

		lastWrittenDicts[pair.ID] = pair.Dict
		if lastDict != nil {
			lastDict.Release()
		}
		pair.Dict.Retain()
	}
	return nil
}

func (w *Writer) start() error {
	w.started = true

	w.mapper.ImportSchema(w.schema)
	w.lastWrittenDicts = make(map[int64]arrow.Array)

	// write out schema payloads
	ps := payloadFromSchema(w.schema, w.mem, &w.mapper)
	defer ps.Release()

	for _, data := range ps {
		err := w.pw.WritePayload(data)
		if err != nil {
			return err
		}
	}

	return nil
}

type dictEncoder struct {
	*recordEncoder
}

func (d *dictEncoder) encodeMetadata(p *Payload, isDelta bool, id, nrows int64) error {
	p.meta = writeDictionaryMessage(d.mem, id, isDelta, nrows, p.size, d.fields, d.meta, d.codec)
	return nil
}

func (d *dictEncoder) Encode(p *Payload, id int64, isDelta bool, dict arrow.Array) error {
	d.start = 0
	defer func() {
		d.start = 0
	}()

	schema := arrow.NewSchema([]arrow.Field{{Name: "dictionary", Type: dict.DataType(), Nullable: true}}, nil)
	batch := array.NewRecord(schema, []arrow.Array{dict}, int64(dict.Len()))
	defer batch.Release()
	if err := d.encode(p, batch); err != nil {
		return err
	}

	return d.encodeMetadata(p, isDelta, id, batch.NumRows())
}

type recordEncoder struct {
	mem memory.Allocator

	fields []fieldMetadata
	meta   []bufferMetadata

	depth      int64
	start      int64
	allow64b   bool
	codec      flatbuf.CompressionType
	compressNP int
}

func newRecordEncoder(mem memory.Allocator, startOffset, maxDepth int64, allow64b bool, codec flatbuf.CompressionType, compressNP int) *recordEncoder {
	return &recordEncoder{
		mem:        mem,
		start:      startOffset,
		depth:      maxDepth,
		allow64b:   allow64b,
		codec:      codec,
		compressNP: compressNP,
	}
}

func (w *recordEncoder) reset() {
	w.start = 0
	w.fields = make([]fieldMetadata, 0)
}

func (w *recordEncoder) compressBodyBuffers(p *Payload) error {
	compress := func(idx int, codec compressor) error {
		if p.body[idx] == nil || p.body[idx].Len() == 0 {
			return nil
		}
		var buf bytes.Buffer
		buf.Grow(codec.MaxCompressedLen(p.body[idx].Len()) + arrow.Int64SizeBytes)
		if err := binary.Write(&buf, binary.LittleEndian, uint64(p.body[idx].Len())); err != nil {
			return err
		}
		codec.Reset(&buf)
		if _, err := codec.Write(p.body[idx].Bytes()); err != nil {
			return err
		}
		if err := codec.Close(); err != nil {
			return err
		}
		p.body[idx] = memory.NewBufferBytes(buf.Bytes())
		return nil
	}

	if w.compressNP <= 1 {
		codec := getCompressor(w.codec)
		for idx := range p.body {
			if err := compress(idx, codec); err != nil {
				return err
			}
		}
		return nil
	}

	var (
		wg          sync.WaitGroup
		ch          = make(chan int)
		errch       = make(chan error)
		ctx, cancel = context.WithCancel(context.Background())
	)
	defer cancel()

	for i := 0; i < w.compressNP; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codec := getCompressor(w.codec)
			for {
				select {
				case idx, ok := <-ch:
					if !ok {
						// we're done, channel is closed!
						return
					}

					if err := compress(idx, codec); err != nil {
						errch <- err
						cancel()
						return
					}
				case <-ctx.Done():
					// cancelled, return early
					return
				}
			}
		}()
	}

	for idx := range p.body {
		ch <- idx
	}

	close(ch)
	wg.Wait()
	close(errch)

	return <-errch
}

func (w *recordEncoder) encode(p *Payload, rec arrow.Record) error {

	// perform depth-first traversal of the row-batch
	for i, col := range rec.Columns() {
		err := w.visit(p, col)
		if err != nil {
			return fmt.Errorf("arrow/ipc: could not encode column %d (%q): %w", i, rec.ColumnName(i), err)
		}
	}

	if w.codec != -1 {
		w.compressBodyBuffers(p)
	}

	// position for the start of a buffer relative to the passed frame of reference.
	// may be 0 or some other position in an address space.
	offset := w.start
	w.meta = make([]bufferMetadata, len(p.body))

	// construct the metadata for the record batch header
	for i, buf := range p.body {
		var (
			size    int64
			padding int64
		)
		// the buffer might be null if we are handling zero row lengths.
		if buf != nil {
			size = int64(buf.Len())
			padding = bitutil.CeilByte64(size) - size
		}
		w.meta[i] = bufferMetadata{
			Offset: offset,
			// even though we add padding, we need the Len to be correct
			// so that decompressing works properly.
			Len: size,
		}
		offset += size + padding
	}

	p.size = offset - w.start
	if !bitutil.IsMultipleOf8(p.size) {
		panic("not aligned")
	}

	return nil
}

func (w *recordEncoder) visit(p *Payload, arr arrow.Array) error {
	if w.depth <= 0 {
		return errMaxRecursion
	}

	if !w.allow64b && arr.Len() > math.MaxInt32 {
		return errBigArray
	}

	if arr.DataType().ID() == arrow.EXTENSION {
		arr := arr.(array.ExtensionArray)
		err := w.visit(p, arr.Storage())
		if err != nil {
			return fmt.Errorf("failed visiting storage of for array %T: %w", arr, err)
		}
		return nil
	}

	if arr.DataType().ID() == arrow.DICTIONARY {
		arr := arr.(*array.Dictionary)
		return w.visit(p, arr.Indices())
	}

	// add all common elements
	w.fields = append(w.fields, fieldMetadata{
		Len:    int64(arr.Len()),
		Nulls:  int64(arr.NullN()),
		Offset: 0,
	})

	if arr.DataType().ID() == arrow.NULL {
		return nil
	}

	switch arr.NullN() {
	case 0:
		// there are no null values, drop the null bitmap
		p.body = append(p.body, nil)
	default:
		data := arr.Data()
		var bitmap *memory.Buffer
		if data.NullN() == data.Len() {
			// every value is null, just use a new unset bitmap to avoid the expense of copying
			bitmap = memory.NewResizableBuffer(w.mem)
			minLength := paddedLength(bitutil.BytesForBits(int64(data.Len())), kArrowAlignment)
			bitmap.Resize(int(minLength))
		} else {
			// otherwise truncate and copy the bits
			bitmap = newTruncatedBitmap(w.mem, int64(data.Offset()), int64(data.Len()), data.Buffers()[0])
		}
		p.body = append(p.body, bitmap)
	}

	switch dtype := arr.DataType().(type) {
	case *arrow.NullType:
		// ok. NullArrays are completely empty.

	case *arrow.BooleanType:
		var (
			data = arr.Data()
			bitm *memory.Buffer
		)

		if data.Len() != 0 {
			bitm = newTruncatedBitmap(w.mem, int64(data.Offset()), int64(data.Len()), data.Buffers()[1])
		}
		p.body = append(p.body, bitm)

	case arrow.FixedWidthDataType:
		data := arr.Data()
		values := data.Buffers()[1]
		arrLen := int64(arr.Len())
		typeWidth := int64(dtype.BitWidth() / 8)
		minLength := paddedLength(arrLen*typeWidth, kArrowAlignment)

		switch {
		case needTruncate(int64(data.Offset()), values, minLength):
			// non-zero offset: slice the buffer
			offset := int64(data.Offset()) * typeWidth
			// send padding if available
			len := minI64(bitutil.CeilByte64(arrLen*typeWidth), int64(values.Len())-offset)
			values = memory.NewBufferBytes(values.Bytes()[offset : offset+len])
		default:
			if values != nil {
				values.Retain()
			}
		}
		p.body = append(p.body, values)

	case *arrow.BinaryType:
		arr := arr.(*array.Binary)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return fmt.Errorf("could not retrieve zero-based value offsets from %T: %w", arr, err)
		}
		data := arr.Data()
		values := data.Buffers()[2]

		var totalDataBytes int64
		if voffsets != nil {
			totalDataBytes = int64(len(arr.ValueBytes()))
		}

		switch {
		case needTruncate(int64(data.Offset()), values, totalDataBytes):
			// slice data buffer to include the range we need now.
			var (
				beg = int64(arr.ValueOffset(0))
				len = minI64(paddedLength(totalDataBytes, kArrowAlignment), int64(totalDataBytes))
			)
			values = memory.NewBufferBytes(data.Buffers()[2].Bytes()[beg : beg+len])
		default:
			if values != nil {
				values.Retain()
			}
		}
		p.body = append(p.body, voffsets)
		p.body = append(p.body, values)

	case *arrow.StringType:
		arr := arr.(*array.String)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return fmt.Errorf("could not retrieve zero-based value offsets from %T: %w", arr, err)
		}
		data := arr.Data()
		values := data.Buffers()[2]

		var totalDataBytes int64
		if voffsets != nil {
			totalDataBytes = int64(len(arr.ValueBytes()))
		}

		switch {
		case needTruncate(int64(data.Offset()), values, totalDataBytes):
			// slice data buffer to include the range we need now.
			var (
				beg = int64(arr.ValueOffset(0))
				len = minI64(paddedLength(totalDataBytes, kArrowAlignment), int64(totalDataBytes))
			)
			values = memory.NewBufferBytes(data.Buffers()[2].Bytes()[beg : beg+len])
		default:
			if values != nil {
				values.Retain()
			}
		}
		p.body = append(p.body, voffsets)
		p.body = append(p.body, values)

	case *arrow.StructType:
		w.depth--
		arr := arr.(*array.Struct)
		for i := 0; i < arr.NumField(); i++ {
			err := w.visit(p, arr.Field(i))
			if err != nil {
				return fmt.Errorf("could not visit field %d of struct-array: %w", i, err)
			}
		}
		w.depth++

	case *arrow.MapType:
		arr := arr.(*array.Map)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return fmt.Errorf("could not retrieve zero-based value offsets for array %T: %w", arr, err)
		}
		p.body = append(p.body, voffsets)

		w.depth--
		var (
			values        = arr.ListValues()
			mustRelease   = false
			values_offset int64
			values_length int64
		)
		defer func() {
			if mustRelease {
				values.Release()
			}
		}()

		if voffsets != nil {
			values_offset = int64(arr.Offsets()[0])
			values_length = int64(arr.Offsets()[arr.Len()]) - values_offset
		}

		if len(arr.Offsets()) != 0 || values_length < int64(values.Len()) {
			// must also slice the values
			values = array.NewSlice(values, values_offset, values_length)
			mustRelease = true
		}
		err = w.visit(p, values)

		if err != nil {
			return fmt.Errorf("could not visit list element for array %T: %w", arr, err)
		}
		w.depth++
	case *arrow.ListType:
		arr := arr.(*array.List)
		voffsets, err := w.getZeroBasedValueOffsets(arr)
		if err != nil {
			return fmt.Errorf("could not retrieve zero-based value offsets for array %T: %w", arr, err)
		}
		p.body = append(p.body, voffsets)

		w.depth--
		var (
			values        = arr.ListValues()
			mustRelease   = false
			values_offset int64
			values_length int64
		)
		defer func() {
			if mustRelease {
				values.Release()
			}
		}()

		if voffsets != nil {
			values_offset = int64(arr.Offsets()[0])
			values_length = int64(arr.Offsets()[arr.Len()]) - values_offset
		}

		if len(arr.Offsets()) != 0 || values_length < int64(values.Len()) {
			// must also slice the values
			values = array.NewSlice(values, values_offset, values_length)
			mustRelease = true
		}
		err = w.visit(p, values)

		if err != nil {
			return fmt.Errorf("could not visit list element for array %T: %w", arr, err)
		}
		w.depth++

	case *arrow.FixedSizeListType:
		arr := arr.(*array.FixedSizeList)

		w.depth--

		size := int64(arr.DataType().(*arrow.FixedSizeListType).Len())
		beg := int64(arr.Offset()) * size
		end := int64(arr.Offset()+arr.Len()) * size

		values := array.NewSlice(arr.ListValues(), beg, end)
		defer values.Release()

		err := w.visit(p, values)

		if err != nil {
			return fmt.Errorf("could not visit list element for array %T: %w", arr, err)
		}
		w.depth++

	default:
		panic(fmt.Errorf("arrow/ipc: unknown array %T (dtype=%T)", arr, dtype))
	}

	return nil
}

func (w *recordEncoder) getZeroBasedValueOffsets(arr arrow.Array) (*memory.Buffer, error) {
	data := arr.Data()
	voffsets := data.Buffers()[1]
	offsetBytesNeeded := arrow.Int32Traits.BytesRequired(data.Len() + 1)

	if data.Offset() != 0 || offsetBytesNeeded < voffsets.Len() {
		// if we have a non-zero offset, then the value offsets do not start at
		// zero. we must a) create a new offsets array with shifted offsets and
		// b) slice the values array accordingly
		//
		// or if there are more value offsets than values (the array has been sliced)
		// we need to trim off the trailing offsets
		shiftedOffsets := memory.NewResizableBuffer(w.mem)
		shiftedOffsets.Resize(offsetBytesNeeded)

		dest := arrow.Int32Traits.CastFromBytes(shiftedOffsets.Bytes())
		offsets := arrow.Int32Traits.CastFromBytes(voffsets.Bytes())[data.Offset() : data.Offset()+data.Len()+1]

		startOffset := offsets[0]
		for i, o := range offsets {
			dest[i] = o - startOffset
		}
		voffsets = shiftedOffsets
	} else {
		voffsets.Retain()
	}
	if voffsets == nil || voffsets.Len() == 0 {
		return nil, nil
	}

	return voffsets, nil
}

func (w *recordEncoder) Encode(p *Payload, rec arrow.Record) error {
	if err := w.encode(p, rec); err != nil {
		return err
	}
	return w.encodeMetadata(p, rec.NumRows())
}

func (w *recordEncoder) encodeMetadata(p *Payload, nrows int64) error {
	p.meta = writeRecordMessage(w.mem, nrows, p.size, w.fields, w.meta, w.codec)
	return nil
}

func newTruncatedBitmap(mem memory.Allocator, offset, length int64, input *memory.Buffer) *memory.Buffer {
	if input == nil {
		return nil
	}

	minLength := paddedLength(bitutil.BytesForBits(length), kArrowAlignment)
	switch {
	case offset != 0 || minLength < int64(input.Len()):
		// with a sliced array / non-zero offset, we must copy the bitmap
		buf := memory.NewResizableBuffer(mem)
		buf.Resize(int(minLength))
		bitutil.CopyBitmap(input.Bytes(), int(offset), int(length), buf.Bytes(), 0)
		return buf
	default:
		input.Retain()
		return input
	}
}

func needTruncate(offset int64, buf *memory.Buffer, minLength int64) bool {
	if buf == nil {
		return false
	}
	return offset != 0 || minLength < int64(buf.Len())
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
