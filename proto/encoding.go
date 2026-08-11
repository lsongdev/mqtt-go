package proto

import (
	"bytes"
	"io"
)

func getUint8(r io.Reader, packetRemaining *int32) uint8 {
	if *packetRemaining < 1 {
		raiseError(dataExceedsPacketError)
	}

	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		raiseError(err)
	}
	*packetRemaining--

	return b[0]
}

func getUint16(r io.Reader, packetRemaining *int32) uint16 {
	if *packetRemaining < 2 {
		raiseError(dataExceedsPacketError)
	}

	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		raiseError(err)
	}
	*packetRemaining -= 2

	return uint16(b[0])<<8 | uint16(b[1])
}

func getUint32(r io.Reader, packetRemaining *int32) uint32 {
	if *packetRemaining < 4 {
		raiseError(dataExceedsPacketError)
	}
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		raiseError(err)
	}
	*packetRemaining -= 4
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func getBinary(r io.Reader, packetRemaining *int32) []byte {
	n := int(getUint16(r, packetRemaining))
	if int(*packetRemaining) < n {
		raiseError(dataExceedsPacketError)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		raiseError(err)
	}
	*packetRemaining -= int32(n)
	return b
}

func getString(r io.Reader, packetRemaining *int32) string {
	strLen := int(getUint16(r, packetRemaining))

	if int(*packetRemaining) < strLen {
		raiseError(dataExceedsPacketError)
	}

	b := make([]byte, strLen)
	if _, err := io.ReadFull(r, b); err != nil {
		raiseError(err)
	}
	*packetRemaining -= int32(strLen)

	return string(b)
}

func setUint8(val uint8, buf *bytes.Buffer) {
	buf.WriteByte(byte(val))
}

func setUint16(val uint16, buf *bytes.Buffer) {
	buf.WriteByte(byte(val & 0xff00 >> 8))
	buf.WriteByte(byte(val & 0x00ff))
}

func setUint32(val uint32, buf *bytes.Buffer) {
	buf.Write([]byte{byte(val >> 24), byte(val >> 16), byte(val >> 8), byte(val)})
}
func setBinary(val []byte, buf *bytes.Buffer) { setUint16(uint16(len(val)), buf); buf.Write(val) }

func setString(val string, buf *bytes.Buffer) {
	length := uint16(len(val))
	setUint16(length, buf)
	buf.WriteString(val)
}

func boolToByte(val bool) byte {
	if val {
		return byte(1)
	}
	return byte(0)
}

func decodeLength(r io.Reader) int32 {
	var v int32
	var buf [1]byte
	var shift uint
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			raiseError(err)
		}

		b := buf[0]
		v |= int32(b&0x7f) << shift

		if b&0x80 == 0 {
			return v
		}
		shift += 7
	}

	raiseError(badLengthEncodingError)
	panic("unreachable")
}

func decodeLengthCounted(r io.Reader, remaining *int32) int32 {
	var v int32
	var b [1]byte
	multiplier := int32(1)
	for i := 0; i < 4; i++ {
		if *remaining < 1 {
			raiseError(dataExceedsPacketError)
		}
		if _, err := io.ReadFull(r, b[:]); err != nil {
			raiseError(err)
		}
		*remaining--
		v += int32(b[0]&127) * multiplier
		if b[0]&128 == 0 {
			return v
		}
		multiplier *= 128
	}
	raiseError(badLengthEncodingError)
	return 0
}

func encodeLength(length int32, buf *bytes.Buffer) {
	if length == 0 {
		buf.WriteByte(0)
		return
	}
	for length > 0 {
		digit := length & 0x7f
		length = length >> 7
		if length > 0 {
			digit = digit | 0x80
		}
		buf.WriteByte(byte(digit))
	}
}
