package mtp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

type imageCaptureFakeWire struct {
	call          func(code uint16, params []uint32, out []byte) ([]byte, []byte, error)
	calls, closes int
}

func (w *imageCaptureFakeWire) command(command, out []byte) ([]byte, []byte, error) {
	w.calls++
	var params []uint32
	for i := 12; i < len(command); i += 4 {
		params = append(params, binary.LittleEndian.Uint32(command[i:]))
	}
	return w.call(binary.LittleEndian.Uint16(command[6:]), params, out)
}
func (w *imageCaptureFakeWire) close() error { w.closes++; return nil }

func imageCaptureTestResponse(code uint16, params ...uint32) []byte {
	b := make([]byte, 12+4*len(params))
	binary.LittleEndian.PutUint32(b, uint32(len(b)))
	binary.LittleEndian.PutUint16(b[4:], 3)
	binary.LittleEndian.PutUint16(b[6:], code)
	binary.LittleEndian.PutUint32(b[8:], 777) // daemon-assigned, deliberately unrelated
	for i, p := range params {
		binary.LittleEndian.PutUint32(b[12+4*i:], p)
	}
	return b
}

func newImageCaptureTestDevice(w ptpCommand) *Device {
	return &Device{proxy: &ptpProxy{wire: w, ops: map[uint16]bool{
		OC_ANDROID_GET_PARTIAL_OBJECT64: true,
		OC_ANDROID_BEGIN_EDIT_OBJECT:    true,
		OC_ANDROID_SEND_PARTIAL_OBJECT:  true,
		OC_ANDROID_TRUNCATE_OBJECT:      true,
		OC_ANDROID_END_EDIT_OBJECT:      true,
	}}}
}

func TestImageCaptureResponseValidationAndNoReplay(t *testing.T) {
	badLength := imageCaptureTestResponse(RC_OK)
	badLength[0] = 16
	badType := imageCaptureTestResponse(RC_OK)
	badType[4] = 2
	for _, b := range [][]byte{nil, {12}, badLength, badType, imageCaptureTestResponse(0), append(imageCaptureTestResponse(RC_OK), 0)} {
		w := &imageCaptureFakeWire{call: func(uint16, []uint32, []byte) ([]byte, []byte, error) { return nil, b, nil }}
		d := newImageCaptureTestDevice(w)
		err := d.RunTransaction(&Container{Code: OC_DeleteObject}, &Container{}, nil, nil, 0, nil)
		if err == nil || d.proxy.wire != nil || w.closes != 1 {
			t.Fatalf("accepted response %x: %v", b, err)
		}
		d.RunTransaction(&Container{Code: OC_DeleteObject}, &Container{}, nil, nil, 0, nil)
		if w.calls != 1 {
			t.Fatal("replayed a write after missing response")
		}
	}
	w := &imageCaptureFakeWire{call: func(uint16, []uint32, []byte) ([]byte, []byte, error) {
		return nil, imageCaptureTestResponse(RC_OK), nil
	}}
	d := newImageCaptureTestDevice(w)
	var rep Container
	if err := d.RunTransaction(&Container{Code: OC_DeleteObject}, &rep, nil, nil, 0, nil); err != nil {
		t.Fatal(err)
	}
	if rep.TransactionID != 777 {
		t.Fatal("discarded daemon transaction ID")
	}
	w.call = func(uint16, []uint32, []byte) ([]byte, []byte, error) {
		return nil, imageCaptureTestResponse(0x2009), nil
	}
	if err := d.RunTransaction(&Container{Code: OC_DeleteObject}, &rep, nil, nil, 0, nil); err != RCError(0x2009) {
		t.Fatal(err)
	}
	if d.proxy.wire == nil {
		t.Fatal("PTP operation error invalidated a healthy session")
	}
	w.call = func(uint16, []uint32, []byte) ([]byte, []byte, error) {
		return nil, imageCaptureTestResponse(0xa801), nil
	}
	if err := d.RunTransaction(&Container{Code: OC_DeleteObject}, &rep, nil, nil, 0, nil); err != RCError(0xa801) || d.proxy.wire == nil {
		t.Fatalf("MTP vendor response rejected: %v", err)
	}
	// Shared response validation must reject an operation error before exposing
	// any buffered proxy data or reporting download progress to the caller.
	var download bytes.Buffer
	w.call = func(uint16, []uint32, []byte) ([]byte, []byte, error) {
		return []byte("failed download"), imageCaptureTestResponse(0x2009), nil
	}
	if err := d.RunTransaction(&Container{Code: OC_GetObject}, &rep, &download, nil, 0, func(int64) error {
		t.Fatal("reported progress for a rejected response")
		return nil
	}); err != RCError(0x2009) || download.Len() != 0 || d.proxy.wire == nil {
		t.Fatalf("exposed data from a rejected response: %v, %q", err, download.String())
	}
	w.call = func(uint16, []uint32, []byte) ([]byte, []byte, error) { return nil, nil, errors.New("timeout") }
	if err := d.RunTransaction(&Container{Code: OC_DeleteObject}, &rep, nil, nil, 0, nil); err == nil || d.proxy.wire != nil {
		t.Fatal("timeout did not invalidate session")
	}
}

func TestImageCaptureChunkedTransferAndEmptyFile(t *testing.T) {
	for _, size := range []int{0, 1024, 1024*1024 - 12, proxyChunkSize + 913} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			original := make([]byte, size)
			for i := range original {
				original[i] = byte(i*31 + 7)
			}
			var stored []byte
			var expectedInitial uint32
			editing := false
			w := &imageCaptureFakeWire{}
			w.call = func(code uint16, params []uint32, out []byte) ([]byte, []byte, error) {
				if len(out) > proxyChunkSize {
					t.Fatalf("unbounded upload: %d", len(out))
				}
				switch code {
				case OC_SendObjectInfo:
					expectedInitial = binary.LittleEndian.Uint32(out[8:])
					return nil, imageCaptureTestResponse(RC_OK, 1, 0, 42), nil
				case OC_SendObject:
					if uint32(len(out)) != expectedInitial || len(out) == 0 {
						t.Fatalf("incorrect initial data: %d vs %d", len(out), expectedInitial)
					}
					stored = append([]byte(nil), out...)
				case OC_ANDROID_BEGIN_EDIT_OBJECT:
					editing = true
				case OC_ANDROID_SEND_PARTIAL_OBJECT:
					if !editing || params[0] != 42 || params[1] != uint32(len(stored)) || params[2] != 0 || params[3] != uint32(len(out)) {
						t.Fatalf("invalid partial write: %v", params)
					}
					stored = append(stored, out...)
					return nil, imageCaptureTestResponse(RC_OK, uint32(len(out))-500), nil
				case OC_ANDROID_TRUNCATE_OBJECT:
					stored = stored[:params[1]]
				case OC_ANDROID_END_EDIT_OBJECT:
					editing = false
				case OC_MTP_GetObjectPropValue:
					b := make([]byte, 8)
					binary.LittleEndian.PutUint64(b, uint64(len(stored)))
					return b, imageCaptureTestResponse(RC_OK), nil
				case OC_ANDROID_GET_PARTIAL_OBJECT64:
					if params[3] > proxyChunkSize {
						t.Fatal("unbounded download")
					}
					b := stored[params[1] : params[1]+params[3]]
					return b, imageCaptureTestResponse(RC_OK, uint32(len(b))), nil
				default:
					t.Fatalf("unexpected command: %x", code)
				}
				return nil, imageCaptureTestResponse(RC_OK), nil
			}
			d := newImageCaptureTestDevice(w)
			_, _, handle, err := d.SendObjectInfo(1, 0, &ObjectInfo{Filename: "test", ObjectFormat: OFC_Undefined, CompressedSize: uint32(size)})
			if err != nil {
				t.Fatal(err)
			}
			var sent int64
			if err := d.SendObject(bytes.NewReader(original), int64(size), func(n int64) error {
				if n < sent {
					t.Fatal("progress decreased")
				}
				sent = n
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if editing || !bytes.Equal(stored, original) || sent != int64(size) {
				t.Fatal("upload mismatch or edit not committed")
			}
			var download bytes.Buffer
			if err := d.GetObject(handle, &download, EmptyProgressFunc); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(download.Bytes(), original) {
				t.Fatal("download mismatch")
			}
		})
	}
}

type imageCaptureShortWriter struct{}

func (imageCaptureShortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestImageCaptureReaderWriterAndCapabilityFailures(t *testing.T) {
	w := &imageCaptureFakeWire{call: func(uint16, []uint32, []byte) ([]byte, []byte, error) {
		return []byte{1, 2}, imageCaptureTestResponse(RC_OK), nil
	}}
	d := newImageCaptureTestDevice(w)
	req := &Container{Code: OC_MTP_GetObjectPropValue}
	if err := d.RunTransaction(req, &Container{}, imageCaptureShortWriter{}, nil, 0, nil); err != io.ErrShortWrite {
		t.Fatal(err)
	}
	if err := d.RunTransaction(req, &Container{}, nil, bytes.NewReader([]byte{1}), 2, nil); err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
	if w.calls != 1 {
		t.Fatal("sent a truncated outgoing payload")
	}
	d.proxy.ops = nil
	for _, size := range []uint32{0, proxyChunkSize + 1} {
		if _, _, _, err := d.SendObjectInfo(1, 0, &ObjectInfo{Filename: "test", ObjectFormat: OFC_Undefined, CompressedSize: size}); err == nil {
			t.Fatal("created an object before checking edit capabilities")
		}
	}
	if w.calls != 1 {
		t.Fatal("unsupported transfer mutated the phone")
	}
}

// Exercise the actual transfer loops across the 4 GiB boundary without
// allocating a multi-gigabyte fixture or accessing a phone.
type imageCaptureZeroReader struct{}

func (imageCaptureZeroReader) Read(b []byte) (int, error) {
	clearImageCaptureTestBytes(b)
	return len(b), nil
}
func clearImageCaptureTestBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func TestImageCapture64BitTransferOffsets(t *testing.T) {
	const size = int64(1<<32 + proxyChunkSize + 17)
	var uploaded, downloaded int64
	buffer := make([]byte, proxyChunkSize)
	w := &imageCaptureFakeWire{call: func(code uint16, params []uint32, out []byte) ([]byte, []byte, error) {
		switch code {
		case OC_SendObjectInfo:
			return nil, imageCaptureTestResponse(RC_OK, 1, 0, 42), nil
		case OC_SendObject:
			uploaded += int64(len(out))
		case OC_ANDROID_SEND_PARTIAL_OBJECT:
			offset := int64(uint64(params[1]) | uint64(params[2])<<32)
			if offset != uploaded {
				t.Fatalf("upload offset=%d, expected %d", offset, uploaded)
			}
			uploaded += int64(len(out))
			return nil, imageCaptureTestResponse(RC_OK, uint32(len(out))), nil
		case OC_MTP_GetObjectPropValue:
			b := make([]byte, 8)
			binary.LittleEndian.PutUint64(b, uint64(size))
			return b, imageCaptureTestResponse(RC_OK), nil
		case OC_ANDROID_GET_PARTIAL_OBJECT64:
			offset := int64(uint64(params[1]) | uint64(params[2])<<32)
			if offset != downloaded {
				t.Fatalf("download offset=%d, expected %d", offset, downloaded)
			}
			downloaded += int64(params[3])
			return buffer[:params[3]], imageCaptureTestResponse(RC_OK, params[3]), nil
		}
		return nil, imageCaptureTestResponse(RC_OK), nil
	}}
	d := newImageCaptureTestDevice(w)
	if _, _, _, err := d.SendObjectInfo(1, 0, &ObjectInfo{Filename: "large", ObjectFormat: OFC_Undefined, CompressedSize: 0xffffffff}); err != nil {
		t.Fatal(err)
	}
	if err := d.SendObject(imageCaptureZeroReader{}, size, EmptyProgressFunc); err != nil {
		t.Fatal(err)
	}
	if err := d.GetObject(42, &NullWriter{}, EmptyProgressFunc); err != nil {
		t.Fatal(err)
	}
	if uploaded != size || downloaded != size {
		t.Fatalf("byte counts %d/%d, expected %d", uploaded, downloaded, size)
	}
}

func TestImageCaptureUploadCancellationFinishesEdit(t *testing.T) {
	var codes []uint16
	w := &imageCaptureFakeWire{call: func(code uint16, _ []uint32, out []byte) ([]byte, []byte, error) {
		codes = append(codes, code)
		if code == OC_SendObjectInfo {
			return nil, imageCaptureTestResponse(RC_OK, 1, 0, 42), nil
		}
		return nil, imageCaptureTestResponse(RC_OK), nil
	}}
	d := newImageCaptureTestDevice(w)
	if _, _, _, err := d.SendObjectInfo(1, 0, &ObjectInfo{Filename: "cancel", ObjectFormat: OFC_Undefined, CompressedSize: proxyChunkSize + 1}); err != nil {
		t.Fatal(err)
	}
	cancelled := errors.New("cancelled")
	if err := d.SendObject(imageCaptureZeroReader{}, proxyChunkSize+1, func(int64) error { return cancelled }); err != cancelled {
		t.Fatal(err)
	}
	if len(codes) != 4 || codes[3] != OC_ANDROID_END_EDIT_OBJECT {
		t.Fatalf("cancellation left edit open or sent another chunk: %x", codes)
	}
}

func TestImageCaptureTransportSelection(t *testing.T) {
	for _, targets := range [][]imageCaptureTarget{nil, {{AppleOwned: false}}} {
		if target, err := imageCaptureMatchingTarget(targets, "", false); target != nil || err != nil {
			t.Fatal("claimed an unowned USB interface")
		}
	}
	if target, err := imageCaptureMatchingTarget([]imageCaptureTarget{{AppleOwned: true, Location: 42}}, "", false); err != nil || target.Location != 42 {
		t.Fatal("lost physical device match")
	}
	if _, err := imageCaptureMatchingTarget([]imageCaptureTarget{{AppleOwned: true}, {AppleOwned: false}}, "", false); err == nil {
		t.Fatal("silently selected among multiple devices")
	}
	targets := []imageCaptureTarget{
		{AppleOwned: true, Manufacturer: "Google", Name: "Pixel", Serial: "test-shared", Location: 42},
		{AppleOwned: false, Name: "Other", Serial: "test-usb", Location: 43},
	}
	if target, err := imageCaptureMatchingTarget(targets, "Google Pixel test-shared", false); err != nil || target == nil || target.Location != 42 {
		t.Fatalf("shared transport did not honor device filter: %v %v", target, err)
	}
	if target, err := imageCaptureMatchingTarget(targets, "test-usb", false); err != nil || target != nil {
		t.Fatal("shared transport intercepted a USB-only match")
	}
	if _, err := imageCaptureMatchingTarget(targets, "[", false); err == nil {
		t.Fatal("invalid device pattern accepted")
	}
	if target, err := imageCaptureMatchingTarget(targets, "test-usb", true); err != nil || target == nil || target.Location != 43 {
		t.Fatalf("forced proxy did not honor device filter: %v %v", target, err)
	}
	if target, err := imageCaptureMatchingTarget(targets[:1], "", true); err != nil || target == nil || target.Location != 42 {
		t.Fatalf("forced proxy lost already-owned device: %v %v", target, err)
	}
	for _, pattern := range []string{"", "missing", "["} {
		if _, err := imageCaptureMatchingTarget(targets, pattern, true); err == nil {
			t.Fatalf("forced proxy accepted ambiguous, missing, or invalid match %q", pattern)
		}
	}
	if _, err := imageCaptureMatchingTarget(nil, "", true); err == nil {
		t.Fatal("forced proxy allowed USB fallback when no camera was found")
	}
}

func TestImageCaptureSessionLifecycle(t *testing.T) {
	w := &imageCaptureFakeWire{call: func(uint16, []uint32, []byte) ([]byte, []byte, error) {
		return nil, imageCaptureTestResponse(RC_OK), nil
	}}
	d := newImageCaptureTestDevice(w)
	d.proxy.info = UsbDeviceInfo{IdVendor: 0x18d1, Product: "phone"}
	if err := d.Open(); err == nil {
		t.Fatal("reopened an active proxy session")
	}
	if err := d.Configure(); err != nil {
		t.Fatal(err)
	}
	if err := d.OpenSession(); err != nil {
		t.Fatal(err)
	}
	if w.calls != 0 {
		t.Fatal("sent a raw session command")
	}
	info, err := d.GetUsbInfo()
	if err != nil || info.IdVendor != 0x18d1 {
		t.Fatalf("USB metadata: %v %v", info, err)
	}
	info.IdVendor = 0
	info, _ = d.GetUsbInfo()
	if info.IdVendor != 0x18d1 {
		t.Fatal("USB metadata was not copied")
	}
	if id, err := d.ID(); err != nil || id != " phone " {
		t.Fatalf("device ID: %q %v", id, err)
	}
	for _, code := range []uint16{OC_OpenSession, OC_CloseSession, OC_ResetDevice} {
		if err := d.RunTransaction(&Container{Code: code}, &Container{}, nil, nil, 0, nil); err == nil {
			t.Fatalf("allowed raw session command %x", code)
		}
	}
	if err := d.DeleteObject(1); err != nil || w.calls != 1 {
		t.Fatalf("ordinary operation: %v, calls=%d", err, w.calls)
	}
	if err := d.CloseSession(); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d.Done()
	d.Done()
	if w.closes != 1 {
		t.Fatalf("closed %d times", w.closes)
	}
	if err := d.Configure(); err == nil {
		t.Fatal("reopened closed proxy")
	}
	if err := d.Open(); err == nil {
		t.Fatal("reacquired a closed proxy session")
	}
	if err := d.DeleteObject(1); err == nil || w.calls != 1 {
		t.Fatal("used closed proxy")
	}
	// Closing a selected but unopened candidate must also prevent a later open.
	unopened := &Device{proxy: &ptpProxy{target: &imageCaptureTarget{}}}
	if err := unopened.Close(); err != nil {
		t.Fatal(err)
	}
	if unopened.proxy.target != nil {
		t.Fatal("retained a native open request after closing")
	}
	if err := unopened.Open(); err == nil {
		t.Fatal("opened a discarded proxy candidate")
	}
}

// Both connections use the same container encoder and response decoder. These
// checks never open a USB handle.
func TestMTPContainerFraming(t *testing.T) {
	req := Container{Code: OC_ANDROID_GET_PARTIAL_OBJECT64, TransactionID: 23, Param: []uint32{7, 8, 1, 4096}}
	command, err := req.commandBytes()
	want := []byte{28, 0, 0, 0, 1, 0, 0xc1, 0x95, 23, 0, 0, 0, 7, 0, 0, 0, 8, 0, 0, 0, 1, 0, 0, 0, 0, 16, 0, 0}
	if err != nil || !bytes.Equal(command, want) {
		t.Fatalf("command framing: %x, %v", command, err)
	}
	req.Param = make([]uint32, 6)
	if _, err := req.commandBytes(); err == nil {
		t.Fatal("encoded more than five parameters")
	}
	var d Device
	var rep Container
	response := imageCaptureTestResponse(RC_OK, 4096)
	var header usbBulkHeader
	if err := binary.Read(bytes.NewReader(response), byteOrder, &header); err != nil {
		t.Fatal(err)
	}
	if err := d.decodeRep(&header, response[usbHdrLen:], &rep); err != nil || len(rep.Param) != 1 || rep.Param[0] != 4096 {
		t.Fatalf("response parameters: %+v, %v", rep, err)
	}
	if err := d.decodeRep(&header, nil, &rep); err == nil {
		t.Fatal("accepted truncated response parameters")
	}
}

func TestImageCaptureDownloadCapabilities(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprint(partial), func(t *testing.T) {
			data := bytes.Repeat([]byte{0xab}, 1024)
			w := &imageCaptureFakeWire{call: func(code uint16, params []uint32, _ []byte) ([]byte, []byte, error) {
				switch code {
				case OC_MTP_GetObjectPropValue:
					b := make([]byte, 8)
					byteOrder.PutUint64(b, uint64(len(data)))
					return b, imageCaptureTestResponse(RC_OK), nil
				case OC_GetPartialObject:
					if !partial || len(params) != 3 || params[0] != 42 || params[1] != 0 || params[2] != uint32(len(data)) {
						t.Fatalf("invalid standard partial read: %v", params)
					}
					return data, imageCaptureTestResponse(RC_OK, uint32(len(data))), nil
				case OC_GetObject:
					if partial {
						t.Fatal("ignored partial-read capability")
					}
					return data, imageCaptureTestResponse(RC_OK), nil
				default:
					t.Fatalf("unexpected command %x", code)
					return nil, nil, nil
				}
			}}
			d := newImageCaptureTestDevice(w)
			d.proxy.ops = map[uint16]bool{OC_GetPartialObject: partial}
			var got bytes.Buffer
			if err := d.GetObject(42, &got, nil); err != nil || !bytes.Equal(got.Bytes(), data) {
				t.Fatalf("download: %v, bytes=%d", err, got.Len())
			}
			w.call = func(uint16, []uint32, []byte) ([]byte, []byte, error) {
				b := make([]byte, 8)
				byteOrder.PutUint64(b, 1<<32+1)
				return b, imageCaptureTestResponse(RC_OK), nil
			}
			if err := d.GetObject(42, &NullWriter{}, nil); err == nil || w.calls != 3 {
				t.Fatal("attempted an unsupported large download")
			}
		})
	}
}

func TestImageCapturePendingUploadFailures(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		w := &imageCaptureFakeWire{call: func(uint16, []uint32, []byte) ([]byte, []byte, error) {
			return nil, imageCaptureTestResponse(RC_OK, 1, 0, 42), nil
		}}
		d := newImageCaptureTestDevice(w)
		info := ObjectInfo{ObjectFormat: OFC_Undefined, CompressedSize: proxyChunkSize + 1}
		if _, _, _, err := d.SendObjectInfo(1, 0, &info); err != nil {
			t.Fatal(err)
		}
		if info.CompressedSize != proxyChunkSize+1 {
			t.Fatal("rewrote caller's object metadata")
		}
		if err := d.DeleteObject(99); err == nil || w.calls != 1 {
			t.Fatal("interleaved a command into a pending upload")
		}
		size := int64(info.CompressedSize)
		if !truncated {
			size--
		}
		if err := d.SendObject(bytes.NewReader([]byte{1}), size, nil); err == nil || d.proxy.wire != nil || w.calls != 1 {
			t.Fatal("sent or retained an invalid pending upload")
		}
	}
}
