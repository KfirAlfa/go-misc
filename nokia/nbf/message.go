package nbf

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"time"
	"unicode/utf16"
)

// predefmessages/1: inbox
// predefmessages/3: outbox

type Message struct {
	// Zip directory information
	Date time.Time
	// Filename information
	Seq          uint32
	Timestamp    uint32
	MultipartSeq uint16
	Flags        uint16
	PartNo       uint8
	PartTotal    uint8
	Peer         [12]byte
	Pad          byte
}

// ParseFilename decomposes the filename of messages found in NBF archives.
// 00001DFC: sequence number of message
// 3CEAC364: Dos timestamp (seconds since 01 Jan 1980, 32-bit integer)
// 00B7: 16-bit multipart sequence number (identical for parts of the same message)
// 2010: 1st byte 0x20 for sms, 0x10 for mms
// 00500000:
// 00302000: for multipart: 2 out of 3.
// 00000000: zero
// 00000000: zero
// 000000000: zero (9 digits)
// 36300XXXXXXX : 12 digit number
// 0000007C : a checksum ?
func (msg *Message) ParseFilename(filename string) (err error) {
	s := filename
	s, msg.Seq, err = getUint32(s)
	if err != nil {
		return err
	}
	s, msg.Timestamp, err = getUint32(s)
	if err != nil {
		return err
	}
	s, n, err := getUint32(s)
	if err != nil {
		return err
	}
	msg.MultipartSeq = uint16(n >> 16)
	msg.Flags = uint16(n)
	s = s[8:] // skip
	s, n, err = getUint32(s)
	if err != nil {
		return err
	}
	msg.PartNo = uint8(n >> 12)
	msg.PartTotal = uint8(n >> 20)
	s = s[25:] // skip
	copy(msg.Peer[:], s[:len(msg.Peer)])
	s = s[len(msg.Peer):]
	msg.Pad = uint8(s[7])
	return nil
}

func getUint32(s string) (rest string, n uint32, err error) {
	x, err := strconv.ParseUint(s[:8], 16, 32)
	return s[8:], uint32(x), err
}

const (
	FLAGS_SMS = 0x2000
	FLAGS_MMS = 0x1000
)

func DosTime(stamp uint32) time.Time {
	t := time.Unix(int64(stamp), 0)
	// Add 10 years
	t = t.Add(3652 * 24 * time.Hour)
	return t
}

// A big-endian interpretation of the binary format.
type rawMessage struct {
	Peer string
	Text string
	// From PDU
	Msg deliverMessage
}

// SMS encoding.
// Inspired by libgammu's libgammu/phone/nokia/dct4s40/6510/6510file.c

// Structure: all integers are big-endian
// u16 u16 u32 u32(size)
// [82]byte (zero)
// [41]uint16 (NUL-terminated peer name)
// PDU (offset is 0xb0)
// 65 unknown bytes
// 0001 0003 size(uint16) [size/2]uint16 (NUL-terminated text)
// 02 size(uint16) + NUL-terminated [size]byte (SMS center)
// 04 0001 002b size(uint16) + [size]byte (NUL-terminated UTF16BE) (peer)
// [23]byte unknown data

func parseMessage(s []byte) (rawMessage, error) {
	// peer (fixed offset 0x5e)
	var runes []uint16
	for off := 0x5e; s[off]|s[off+1] != 0; off += 2 {
		runes = append(runes, binary.BigEndian.Uint16(s[off:off+2]))
	}
	peer := string(utf16.Decode(runes))

	// PDU frame starts at 0xb0
	// incoming PDU frame:
	// * NN 91 <NN/2 bytes> (NN : number of BCD digits, little endian)
	//   source number, padded with 0xf halfbyte.
	// * 00 FF (data format, GSM 03.40 section 9.2.3.10)
	// * YY MM DD HH MM SS ZZ (BCD date time, little endian)
	// * NN <NN septets> (NN : number of packed 7-bit data)
	// received SMS: 04 0b 91
	pdu := s[0xb0:]
	msgType := pdu[0]
	var msg deliverMessage
	switch msgType & 3 {
	case 0: // SMS-DELIVER
		var n int
		msg, n = parseDeliverMessage(pdu)
		pdu = pdu[n:]
	case 1: // SMS-SUBMIT
	case 2: // SMS-COMMAND
	case 3: // reserved
		panic("invalid message type 3")
	}
	// END of PDU.
	if len(pdu) == 0 {
		return rawMessage{Peer: peer, Msg: msg}, nil
	}
	if len(pdu) < 72 {
		return rawMessage{}, fmt.Errorf("truncated message")
	}
	pdu = pdu[65:]
	length := int(pdu[5])
	pdu = pdu[6:]
	text := make([]rune, length/2)
	for i := range text {
		text[i] = rune(binary.BigEndian.Uint16(pdu[2*i : 2*i+2]))
	}
	//log.Printf("%q", string(text))

	m := rawMessage{
		Peer: peer,
		Text: string(text),
		Msg:  msg,
	}
	return m, nil
}

// Parsing of DELIVER-MESSAGE

// A deliverMessage represents the contents of a SMS-DELIVER message
// as per GSM 03.40 TPDU specification.
type deliverMessage struct {
	MsgType  byte
	MoreMsg  bool // true encoded as zero
	FromAddr string
	Protocol byte
	// Coding byte
	Compressed bool
	Unicode    bool
	SMSCStamp  time.Time

	RawData []byte // UCS-2 encoded text, unpacked 7-bit data.

	// Concatenated SMS
	Concat            bool
	Ref, Part, NParts int
}

func (msg deliverMessage) UserData() string {
	if msg.Unicode {
		runes := make([]uint16, len(msg.RawData)/2)
		for i := range runes {
			hi, lo := msg.RawData[2*i], msg.RawData[2*i+1]
			runes[i] = uint16(hi)<<8 | uint16(lo)
		}
		return string(utf16.Decode(runes))
	} else {
		return translateSMS(msg.RawData, &basicSMSset)
	}
}
func parseDeliverMessage(s []byte) (msg deliverMessage, size int) {
	p := s
	msg.MsgType = p[0] & 3
	msg.MoreMsg = p[0]&4 == 0
	nbLen := int(p[1])
	msg.FromAddr = decodeBCD(p[3 : 3+(nbLen+1)/2])
	//log.Printf("number: %s", number)
	size += 3 + (nbLen+1)/2
	p = s[size:]

	// Format
	format := p[1]
	msg.Compressed = format&0x20 != 0
	msg.Unicode = format&8 != 0

	// Date time
	msg.SMSCStamp = parseDateTime(p[2:9])
	size += 2 + 7
	p = s[size:]

	// Payload
	if msg.Unicode {
		// Unicode (70 UCS-2 characters in 140 bytes)
		length := int(p[0]) // length in bytes
		msg.RawData = p[1 : length+1]
		size += length + 1
	} else {
		// 7-bit encoded format (160 septets in 140 bytes)
		length := int(p[0]) // length in septets
		packedLen := length - length/8
		msg.RawData = unpack7bit(p[1 : 1+packedLen])
		msg.RawData = msg.RawData[:length]
		size += packedLen + 1
	}
	ud := p[1:]
	switch {
	case len(ud) >= 6 && ud[0] == 5 && ud[1] == 0 && ud[2] == 3:
		// Concatenated SMS data starts with 0x05 0x00 0x03 Ref NPart Part
		msg.Concat = true
		msg.Part = int(ud[5])
		msg.NParts = int(ud[4])
		msg.Ref = int(ud[3])
		if msg.Unicode {
			msg.RawData = msg.RawData[6:]
		} else {
			msg.RawData = msg.RawData[7:] // remove initial 48 bits
		}
	case len(ud) >= 7 && ud[0] == 6 && ud[1] == 8 && ud[2] == 4:
		// Concatenated SMS data with 16-bit ref number.
		msg.Concat = true
		msg.Part = int(ud[6])
		msg.NParts = int(ud[5])
		msg.Ref = int(ud[3])<<8 | int(ud[4])
		if msg.Unicode {
			msg.RawData = msg.RawData[7:]
		} else {
			msg.RawData = msg.RawData[8:] // remove initial 56 bits
		}
	}
	return
}

// Ref: GSM 03.40 section 9.2.3.11
func parseDateTime(b []byte) time.Time {
	var dt [7]int
	for i := range dt {
		dt[i] = int(b[i]&0xf)*10 + int(b[i]>>4)
	}
	return time.Date(
		2000+dt[0],
		time.Month(dt[1]),
		dt[2],
		dt[3], dt[4], dt[5], 0, time.FixedZone("", dt[6]*3600/4))
}

func decodeBCD(b []byte) string {
	s := make([]byte, 0, len(b)*2)
	for _, c := range b {
		s = append(s, '0'+(c&0xf))
		if c>>4 == 0xf {
			break
		} else {
			s = append(s, '0'+(c>>4))
		}
	}
	return string(s)
}

func unpack7bit(s []byte) []byte {
	// each byte may contain a part of septet i in lower bits
	// and septet i+1 in higher bits.
	buf := uint16(0)
	buflen := uint(0)
	out := make([]byte, 0, len(s)+len(s)/7+1)
	for len(s) > 0 {
		buf |= uint16(s[0]) << buflen
		buflen += 8
		s = s[1:]
		for buflen >= 7 {
			out = append(out, byte(buf&0x7f))
			buflen -= 7
			buf >>= 7
		}
	}
	return out
}

// translateSMS decodes a 7-bit encoded SMS text into a standard
// UTF-8 encoded string.
func translateSMS(s []byte, charset *[128]rune) string {
	r := make([]rune, len(s))
	for i, b := range s {
		r[i] = charset[b]
	}
	return string(r)
}

// See http://en.wikipedia.org/wiki/GSM_03.38

var basicSMSset = [128]rune{
	// 0x00
	'@', '£', '$', '¥', 'è', 'é', 'ù', 'ì',
	'ò', 'Ç', '\n', 'Ø', 'ø', '\r', 'Å', 'å',
	// 0x10
	'Δ', '_', 'Φ', 'Γ', 'Λ', 'Ω', 'Π', 'Ψ',
	'Σ', 'Θ', 'Ξ', -1 /* ESC */, 'Æ', 'æ', 'ß', 'É',
	// 0x20
	' ', '!', '"', '#', '¤', '%', '&', '\'',
	'(', ')', '*', '+', ',', '-', '.', '/',
	// 0x30
	'0', '1', '2', '3', '4', '5', '6', '7',
	'8', '9', ':', ';', '<', '=', '>', '?',
	// 0x40
	'¡', 'A', 'B', 'C', 'D', 'E', 'F', 'G',
	'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O',
	// 0x50
	'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W',
	'X', 'Y', 'Z', 'Ä', 'Ö', 'Ñ', 'Ü', '§',
	// 0x60
	'¿', 'a', 'b', 'c', 'd', 'e', 'f', 'g',
	'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o',
	// 0x70
	'p', 'q', 'r', 's', 't', 'u', 'v', 'w',
	'x', 'y', 'z', 'ä', 'ö', 'ñ', 'ü', 'à',
}
