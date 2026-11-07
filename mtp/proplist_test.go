package mtp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestDecodeObjectPropList(t *testing.T) {
	var b bytes.Buffer
	w := func(v interface{}) { binary.Write(&b, byteOrder, v) }

	w(uint32(3))
	// handle 7: filename "a.jpg"
	w(uint32(7))
	w(uint16(OPC_ObjectFileName))
	w(uint16(DTC_STR))
	str, _ := encodeStr(make([]byte, 2*5+3), "a.jpg")
	b.Write(str)
	// handle 7: size 5 GiB (does not fit ObjectInfo.CompressedSize)
	w(uint32(7))
	w(uint16(OPC_ObjectSize))
	w(uint16(DTC_UINT64))
	w(uint64(5 << 30))
	// handle 8: format
	w(uint32(8))
	w(uint16(OPC_ObjectFormat))
	w(uint16(DTC_UINT16))
	w(uint16(OFC_Association))

	got, err := decodeObjectPropList(&b)
	if err != nil {
		t.Fatal(err)
	}

	want := []ObjectPropListElement{
		{7, OPC_ObjectFileName, DTC_STR, "a.jpg"},
		{7, OPC_ObjectSize, DTC_UINT64, uint64(5 << 30)},
		{8, OPC_ObjectFormat, DTC_UINT16, uint16(OFC_Association)},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d elements, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("element %d: got %#v, want %#v", i, got[i], want[i])
		}
	}

	if _, err := decodeObjectPropList(bytes.NewReader([]byte{1, 0, 0, 0, 7, 0, 0, 0, 4, 0xdc, 0x04, 0x40})); err == nil {
		t.Error("array data type should be rejected, not misparsed")
	}
}
