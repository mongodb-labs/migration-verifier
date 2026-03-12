package keystring

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/bits"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// This is a direct port of the Javascript keystring decoder from
// https://github.com/mongodb-js/mongodb-resumetoken-decoder/blob/8d113be6f4d6aec69f17ce0546af7e69d40800b7/src/keystringdecoder.ts
// -- Original Javascript comment
// This code is adapted from
// https://github.com/mongodb/mongo/blob/6e34f5094204aaf9bf14b51252ab347b4035b584/src/mongo/db/storage/key_string.cpp
// with the omission of support for inverted fields (i.e. different ordering),
// type information, and no full Decimal128 support.
// -- End Original Javascript comment

type cType int

const (
	kMinKey        cType = 10
	kUndefined     cType = 15
	kNullish       cType = 20
	kNumeric       cType = 30
	kStringLike    cType = 60
	kObject        cType = 70
	kArray         cType = 80
	kBinData       cType = 90
	kOID           cType = 100
	kBool          cType = 110
	kDate          cType = 120
	kTimestamp     cType = 130
	kRegEx         cType = 140
	kDBRef         cType = 150
	kCode          cType = 160
	kCodeWithScope cType = 170
	kMaxKey        cType = 240

	kNumericNaN                    cType = kNumeric + 0
	kNumericNegativeLargeMagnitude cType = kNumeric + 1 // <= -2**63 including -Inf
	kNumericNegative8ByteInt       cType = kNumeric + 2
	kNumericNegative7ByteInt       cType = kNumeric + 3
	kNumericNegative6ByteInt       cType = kNumeric + 4
	kNumericNegative5ByteInt       cType = kNumeric + 5
	kNumericNegative4ByteInt       cType = kNumeric + 6
	kNumericNegative3ByteInt       cType = kNumeric + 7
	kNumericNegative2ByteInt       cType = kNumeric + 8
	kNumericNegative1ByteInt       cType = kNumeric + 9
	kNumericNegativeSmallMagnitude cType = kNumeric + 10 // between 0 and -1 exclusive
	kNumericZero                   cType = kNumeric + 11
	kNumericPositiveSmallMagnitude cType = kNumeric + 12 // between 0 and 1 exclusive
	kNumericPositive1ByteInt       cType = kNumeric + 13
	kNumericPositive2ByteInt       cType = kNumeric + 14
	kNumericPositive3ByteInt       cType = kNumeric + 15
	kNumericPositive4ByteInt       cType = kNumeric + 16
	kNumericPositive5ByteInt       cType = kNumeric + 17
	kNumericPositive6ByteInt       cType = kNumeric + 18
	kNumericPositive7ByteInt       cType = kNumeric + 19
	kNumericPositive8ByteInt       cType = kNumeric + 20
	kNumericPositiveLargeMagnitude cType = kNumeric + 21 // >= 2**63 including +Inf

	kBoolFalse cType = kBool + 0
	kBoolTrue  cType = kBool + 1
)

const kEnd = 4
const kLess = 1
const kGreater = 254

func numBytesForInt(ctype cType) int {
	if ctype >= kNumericPositive1ByteInt {
		return int(ctype - kNumericPositive1ByteInt + 1)
	}

	return int(kNumericNegative1ByteInt - ctype + 1)
}

type bufferConsumer struct {
	buf   []uint8
	index int
}

func NewBufferConsumer(inbuf []uint8) bufferConsumer {
	return bufferConsumer{
		buf:   inbuf,
		index: 0,
	}
}

func (bc bufferConsumer) peekUint8() uint8 {
	return bc.buf[bc.index]
}

func (bc bufferConsumer) hasUint8() bool {
	return bc.index < len(bc.buf)
}

func (bc *bufferConsumer) readUint8() (uint8, error) {
	if bc.index >= len(bc.buf) {
		return 0, errors.New("unexpected end of input")
	}
	bc.index++
	return bc.buf[bc.index-1], nil
}

func (bc *bufferConsumer) readUint32BE() (uint32, error) {
	b3, err := bc.readUint8()
	if err != nil {
		return 0, err
	}
	b2, err := bc.readUint8()
	if err != nil {
		return 0, err
	}
	b1, err := bc.readUint8()
	if err != nil {
		return 0, err
	}
	b0, err := bc.readUint8()
	if err != nil {
		return 0, err
	}
	return (uint32(b3) << 24) | (uint32(b2) << 16) | uint32(b1)<<8 | uint32(b0), nil
}

func (bc *bufferConsumer) readUint64BE() (uint64, error) {
	w1, err := bc.readUint32BE()
	if err != nil {
		return 0, err
	}
	w0, err := bc.readUint32BE()
	if err != nil {
		return 0, err
	}
	return (uint64(w1) << 32) | uint64(w0), nil
}

func (bc *bufferConsumer) readBytes(n int) ([]uint8, error) {
	if bc.index+n > len(bc.buf) {
		return nil, errors.New("unexpected end of input")
	}
	ret := bc.buf[bc.index : bc.index+n]
	bc.index += n
	return ret, nil
}

func (bc *bufferConsumer) readCString() (string, error) {
	length := bytes.IndexByte(bc.buf[bc.index:], 0)
	if length == -1 {
		length = len(bc.buf) - bc.index
	}
	bytestr, err := bc.readBytes(length)
	if err != nil {
		return "", err
	}
	_, err = bc.readUint8() // The \0 byte
	if err != nil {
		return "", err
	}
	return string(bytestr), nil
}

func (bc *bufferConsumer) readCStringWithNuls() (string, error) {
	str, err := bc.readCString()
	if err != nil {
		return "", err
	}
	for bc.hasUint8() && (bc.peekUint8() == 0xff) {
		_, _ = bc.readUint8()
		nextStr, err := bc.readCString()
		if err != nil {
			return "", err
		}
		str += "\000" + nextStr
	}
	return str, nil
}

func read64BitIEEE754(val uint64) float64 {
	return math.Float64frombits(val)
}

type KeyStringVersion int

const (
	V0 KeyStringVersion = 0
	V1 KeyStringVersion = 1
)

type decodeMode int

const (
	modeTopLevel = iota
	modeSingle   = iota
	modeNamed    = iota
)

func readValue(ctype cType, version KeyStringVersion, buf *bufferConsumer) (any, error) {
	isNegative := false
	switch ctype {
	case kMinKey:
		return bson.MinKey{}, nil
	case kMaxKey:
		return bson.MaxKey{}, nil
	case kNullish:
		return nil, nil
	case kUndefined:
		return bson.Undefined{}, nil
	case kBoolTrue:
		return true, nil
	case kBoolFalse:
		return false, nil
	case kDate:
		dateTimeAsInt, err := buf.readUint64BE()
		if err != nil {
			return nil, err
		}
		return bson.DateTime(dateTimeAsInt ^ (uint64(1) << 63)), nil
	case kTimestamp:
		t, err := buf.readUint32BE()
		if err != nil {
			return nil, err
		}
		i, err := buf.readUint32BE()
		if err != nil {
			return nil, err
		}
		return bson.Timestamp{T: t, I: i}, nil
	case kOID:
		var oid bson.ObjectID
		oidSlice, err := buf.readBytes(12)
		if err != nil {
			return nil, err
		}
		copy(oid[:], oidSlice)
		return oid, nil
	case kStringLike:
		return buf.readCStringWithNuls()
	case kCode:
		str, err := buf.readCStringWithNuls()
		if err != nil {
			return nil, err
		}
		return bson.JavaScript(str), nil
	case kCodeWithScope:
		code, err := buf.readCStringWithNuls()
		if err != nil {
			return nil, err
		}
		scope, err := keystringToBsonPartial(version, buf, modeNamed)
		if err != nil {
			return nil, err
		}
		return bson.CodeWithScope{Code: bson.JavaScript(code), Scope: scope}, nil
	case kBinData:
		size8, err := buf.readUint8()
		if err != nil {
			return nil, err
		}
		size := int(size8)
		if size == 0xff {
			size32, err := buf.readUint32BE()
			if err != nil {
				return nil, err
			}
			size = int(size32)
		}
		subtype, err := buf.readUint8()
		if err != nil {
			return nil, err
		}
		data, err := buf.readBytes(size)
		if err != nil {
			return nil, err
		}
		return bson.Binary{Data: data, Subtype: subtype}, nil
	case kRegEx:
		pattern, err := buf.readCString()
		if err != nil {
			return nil, err
		}
		flags, err := buf.readCString()
		if err != nil {
			return nil, err
		}
		return bson.Regex{Pattern: pattern, Options: flags}, nil
	case kDBRef:
		size, err := buf.readUint32BE()
		if err != nil {
			return nil, err
		}
		nsBytes, err := buf.readBytes(int(size))
		if err != nil {
			return nil, err
		}
		ns := string(nsBytes)
		oidBytes, err := buf.readBytes(12)
		if err != nil {
			return nil, err
		}
		var oid bson.ObjectID
		copy(oid[:], oidBytes)
		return bson.DBPointer{DB: ns, Pointer: oid}, nil // TODO: What happens to non-OID DBRefs?
	case kObject:
		return keystringToBsonPartial(version, buf, modeNamed)
	case kArray:
		arr := bson.A{}
		for buf.hasUint8() && buf.peekUint8() != 0 {
			nextEl, err := keystringToBsonPartial(version, buf, modeSingle)
			if err != nil {
				return nil, err
			}
			arr = append(arr, nextEl)
		}
		_, err := buf.readUint8()
		if err != nil {
			return nil, err
		}
		return arr, nil
	case kNumericNaN:
		return math.NaN(), nil
	case kNumericZero:
		return 0, nil
	case kNumericNegativeLargeMagnitude:
		isNegative = true
		fallthrough
	case kNumericPositiveLargeMagnitude:
		encoded, err := buf.readUint64BE()
		if err != nil {
			return nil, err
		}
		if isNegative {
			encoded = ^encoded
		}
		if version == V0 {
			return read64BitIEEE754(encoded), nil
		} else if (encoded & (uint64(1) << 63)) == 0 { // In range of (finite) doubles
			hasDecimalContinuation := (encoded & uint64(1)) != 0
			encoded >>= 1              // remove decimal continuation marker
			encoded |= uint64(1) << 62 // implied leading exponent bit
			bin := read64BitIEEE754(encoded)
			if isNegative {
				bin = -bin
			}
			if hasDecimalContinuation {
				// note Decimal128 is not supported.  Go supports it so we could add it.
				_, err = buf.readUint64BE()
				if err != nil {
					return nil, err
				}
			}
			return bin, nil
		} else if encoded == 0xffff_ffff_ffff_ffff { // Infinity
			if isNegative {
				return math.Inf(-1), nil
			} else {
				return math.Inf(1), nil
			}
		} else {
			// Decimal128 of too-large magnitude
			_, err = buf.readUint64BE() // low bits
			if err != nil {
				return nil, err
			}
			if isNegative {
				return math.Inf(-1), nil
			} else {
				return math.Inf(1), nil
			}
		}
	case kNumericNegativeSmallMagnitude:
		isNegative = true
		fallthrough
	case kNumericPositiveSmallMagnitude:
		encoded, err := buf.readUint64BE()
		if err != nil {
			return nil, err
		}
		if isNegative {
			encoded = ^encoded
		}
		if version == V0 {
			return read64BitIEEE754(encoded), nil
		}
		switch encoded >> 62 {
		case 0:
			// Teeny tiny decimal, smaller magnitude than 2**(-1074)
			_, err = buf.readUint64BE()
			if err != nil {
				return nil, err
			}
			return 0, nil
		case 1:
			fallthrough
		case 2:
			hasDecimalContinuation := (encoded & 1) != 0
			encoded -= uint64(1) << 62
			encoded >>= 1
			scaledBin := read64BitIEEE754(encoded)
			// Javascript was
			// const bin = scaledBin * (1 ** -256);
			// which I think is wrong.
			bin := math.Ldexp(scaledBin, -256)
			if hasDecimalContinuation {
				_, err = buf.readUint64BE()
				if err != nil {
					return nil, err
				}
			}
			if isNegative {
				return -bin, nil
			}
			return bin, nil
		case 3:
			encoded >>= 2
			bin := read64BitIEEE754(encoded)
			if isNegative {
				return -bin, nil
			}
			return bin, nil
		default:
			return nil, errors.New("unreachable")
		}
	case kNumericNegative8ByteInt:
		fallthrough
	case kNumericNegative7ByteInt:
		fallthrough
	case kNumericNegative6ByteInt:
		fallthrough
	case kNumericNegative5ByteInt:
		fallthrough
	case kNumericNegative4ByteInt:
		fallthrough
	case kNumericNegative3ByteInt:
		fallthrough
	case kNumericNegative2ByteInt:
		fallthrough
	case kNumericNegative1ByteInt:
		isNegative = true
		fallthrough // (format is the same as positive, but inverted)
	case kNumericPositive1ByteInt:
		fallthrough
	case kNumericPositive2ByteInt:
		fallthrough
	case kNumericPositive3ByteInt:
		fallthrough
	case kNumericPositive4ByteInt:
		fallthrough
	case kNumericPositive5ByteInt:
		fallthrough
	case kNumericPositive6ByteInt:
		fallthrough
	case kNumericPositive7ByteInt:
		fallthrough
	case kNumericPositive8ByteInt:
		var encodedIntegerPart uint64
		{
			intBytesRemaining := numBytesForInt(ctype)
			for ; intBytesRemaining != 0; intBytesRemaining-- {
				myByte, err := buf.readUint8()
				if err != nil {
					return nil, err
				}
				if isNegative {
					myByte = ^myByte
				}
				encodedIntegerPart = (encodedIntegerPart << 8) | uint64(myByte)
			}
		}
		haveFractionalPart := (encodedIntegerPart & 1) != 0
		integerPart := int64(encodedIntegerPart >> 1)
		if !haveFractionalPart {
			maxint32 := (int64(1) << 31)
			fitsInInt32 := integerPart < maxint32 || (isNegative && integerPart == maxint32)
			if isNegative {
				integerPart = -integerPart
			}
			if fitsInInt32 {
				return int32(integerPart), nil
			}
			return integerPart, nil
		}

		if version == V0 {
			// KeyString V0: anything fractional is a double
			exponent := (64 - bits.LeadingZeros64(uint64(integerPart))) - 1
			fractionalBits := 52 - exponent
			fractionalBytes := (fractionalBits + 7) / 8

			doubleBits := uint64(integerPart) << fractionalBits
			doubleBits &= ^(uint64(1) << 52)
			doubleBits |= (uint64(exponent) + 1023) << 52
			if isNegative {
				doubleBits |= (uint64(1) << 63)
			}
			for i := 0; i < fractionalBytes; i++ {
				myByte, err := buf.readUint8()
				if err != nil {
					return nil, err
				}
				if isNegative {
					myByte = ^myByte
				}
				doubleBits |= uint64(myByte) << ((fractionalBytes - i - 1) * 8)
			}
			return read64BitIEEE754(doubleBits), nil
		}

		// KeyString V1: all numeric values with fractions have at least 8 bytes.
		// Start with integer part, and read until we have a full 8 bytes worth of data.
		fracBytes := 8 - numBytesForInt(ctype)
		encodedFraction := uint64(integerPart)
		for fracBytesRemaining := fracBytes; fracBytesRemaining != 0; fracBytesRemaining-- {
			myByte, err := buf.readUint8()
			if err != nil {
				return nil, err
			}
			if isNegative {
				myByte = ^myByte
			}
			encodedFraction = (encodedFraction << 8) | uint64(myByte)
		}

		// Zero out the DCM and convert the whole binary fraction
		bin := math.Ldexp(float64(encodedFraction & ^uint64(3)), -8*fracBytes)
		dcm := 3
		if fracBytes > 0 {
			dcm = int(encodedFraction & 3)
		}
		if dcm != 0 && dcm != 2 {
			_, err := buf.readUint64BE()
			if err != nil {
				return nil, err
			}
		}
		if isNegative {
			return -bin, nil
		}
		return bin, nil
	default:
		return nil, fmt.Errorf("unknown keystring ctype %v", ctype)
	}
}

func keystringToBsonPartial(version KeyStringVersion, buf *bufferConsumer, mode decodeMode) (any, error) {
	contents := bson.D{}
	for buf.hasUint8() {
		b, _ := buf.readUint8()
		ctype := cType(b)
		if ctype == kLess || ctype == kGreater {
			b, err := buf.readUint8()
			if err != nil {
				return nil, err
			}
			ctype = cType(b)
		}
		if ctype == kEnd {
			break
		}
		if mode == modeNamed {
			if ctype == 0 {
				break
			}
			key, err := buf.readCString()
			if err != nil {
				return nil, err
			}
			b, err := buf.readUint8() // again ctype, but more accurate this time
			if err != nil {
				return nil, err
			}
			ctype = cType(b)
			value, err := readValue(ctype, version, buf)
			if err != nil {
				return nil, err
			}
			contents = append(contents, bson.E{Key: key, Value: value})
		} else if mode == modeSingle {
			return readValue(ctype, version, buf)
		} else {
			value, err := readValue(ctype, version, buf)
			if err != nil {
				return nil, err
			}
			contents = append(contents, bson.E{"", value})
		}
	}
	return contents, nil
}

func KeystringToBson(version KeyStringVersion, inbuf any) (bson.D, error) {
	buf, ok := inbuf.([]uint8)
	if !ok {
		strbuf, ok := inbuf.(string)
		if !ok {
			return nil, errors.New("KeystringToBson accepts only []uint8 or string")
		}
		var err error
		buf, err = hex.DecodeString(strbuf)
		if err != nil {
			return nil, err
		}
	}
	bc := NewBufferConsumer(buf)
	result, err := keystringToBsonPartial(version, &bc, modeTopLevel)
	if err != nil {
		return nil, err
	}
	return result.(bson.D), nil
}
