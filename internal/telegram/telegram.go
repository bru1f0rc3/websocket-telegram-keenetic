package telegram

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"math"
	"net"
)

// IP ranges owned by Telegram
var tgRanges [][2]uint32

func init() {
	for _, r := range [][2]string{
		{"185.76.151.0", "185.76.151.255"},
		{"149.154.160.0", "149.154.175.255"},
		{"91.105.192.0", "91.105.193.255"},
		{"91.108.0.0", "91.108.255.255"},
	} {
		lo := binary.BigEndian.Uint32(net.ParseIP(r[0]).To4())
		hi := binary.BigEndian.Uint32(net.ParseIP(r[1]).To4())
		tgRanges = append(tgRanges, [2]uint32{lo, hi})
	}
}

func IsTelegramIP(ip string) bool {
	p := net.ParseIP(ip)
	if p == nil {
		return false
	}
	p = p.To4()
	if p == nil {
		return false
	}
	n := binary.BigEndian.Uint32(p)
	for _, r := range tgRanges {
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

type DCInfo struct {
	DC      int
	IsMedia bool
}

var IPtoDC = map[string]DCInfo{
	// DC1
	"149.154.175.50": {1, false}, "149.154.175.51": {1, false},
	"149.154.175.53": {1, false}, "149.154.175.54": {1, false},
	"149.154.175.52": {1, true},
	// DC2
	"149.154.167.41": {2, false}, "149.154.167.50": {2, false},
	"149.154.167.51": {2, false}, "149.154.167.220": {2, false},
	"95.161.76.100":   {2, false},
	"149.154.167.151": {2, true}, "149.154.167.222": {2, true},
	"149.154.167.223": {2, true}, "149.154.162.123": {2, true},
	// DC3
	"149.154.175.100": {3, false}, "149.154.175.101": {3, false},
	"149.154.175.102": {3, true},
	// DC4
	"149.154.167.91": {4, false}, "149.154.167.92": {4, false},
	"149.154.164.250": {4, true}, "149.154.166.120": {4, true},
	"149.154.166.121": {4, true}, "149.154.167.118": {4, true},
	"149.154.165.111": {4, true},
	// DC5
	"91.108.56.100": {5, false}, "91.108.56.101": {5, false},
	"91.108.56.116": {5, false}, "91.108.56.126": {5, false},
	"149.154.171.5": {5, false},
	"91.108.56.102": {5, true}, "91.108.56.128": {5, true},
	"91.108.56.151": {5, true},
	// DC203
	"91.105.192.100": {203, false},
}

var DCOverrides = map[int]int{203: 2}

// DefaultDCIPs contains canonical WebSocket-capable IPs for Telegram DCs.
// DC2 and DC4 share the .220 endpoint which serves WebSocket connections.
// DC1, DC3, DC5 don't have dedicated WS IPs — they get 302 redirects
// and fall back to direct TCP automatically.
var DefaultDCIPs = map[int]string{
	2: "149.154.167.220",
	4: "149.154.167.220",
}

// AutoFillDCs returns a new map with all 5 DCs populated.
// User-provided IPs override the defaults. This ensures that
// traffic to DC1/3/5 goes through WS instead of slow TCP fallback.
func AutoFillDCs(dcOpt map[int]string) map[int]string {
	filled := make(map[int]string, len(DefaultDCIPs))
	for dc, ip := range DefaultDCIPs {
		filled[dc] = ip
	}
	for dc, ip := range dcOpt {
		filled[dc] = ip
	}
	return filled
}

var validProtos = map[uint32]bool{
	0xEFEFEFEF: true, 0xEEEEEEEE: true, 0xDDDDDDDD: true,
}

// DCFromInit extracts DC ID from 64-byte MTProto obfuscation init packet.
func DCFromInit(data []byte) (dc int, isMedia bool, ok bool) {
	if len(data) < 64 {
		return
	}
	block, err := aes.NewCipher(data[8:40])
	if err != nil {
		return
	}
	ks := make([]byte, 64)
	cipher.NewCTR(block, data[40:56]).XORKeyStream(ks, ks)

	plain := make([]byte, 8)
	for i := 0; i < 8; i++ {
		plain[i] = data[56+i] ^ ks[56+i]
	}
	proto := binary.LittleEndian.Uint32(plain[0:4])
	if !validProtos[proto] {
		return
	}
	dcRaw := int16(binary.LittleEndian.Uint16(plain[4:6]))
	dc = int(dcRaw)
	isMedia = dc < 0
	if dc < 0 {
		dc = -dc
	}
	if (dc >= 1 && dc <= 5) || dc == 203 {
		ok = true
	}
	return
}

// PatchInitDC patches dc_id in the 64-byte MTProto init packet.
func PatchInitDC(data []byte, dc int, isMedia bool) []byte {
	if len(data) < 64 {
		return data
	}
	block, err := aes.NewCipher(data[8:40])
	if err != nil {
		return data
	}
	ks := make([]byte, 64)
	cipher.NewCTR(block, data[40:56]).XORKeyStream(ks, ks)

	out := make([]byte, len(data))
	copy(out, data)
	dcVal := int16(dc)
	if isMedia {
		dcVal = -dcVal
	}
	b := make([]byte, 2)
	binary.LittleEndian.PutUint16(b, uint16(dcVal))
	out[60] = ks[60] ^ b[0]
	out[61] = ks[61] ^ b[1]
	return out
}

// MsgSplitter splits multiplexed MTProto messages for per-frame WS sending.
type MsgSplitter struct {
	stream cipher.Stream
}

func NewMsgSplitter(initData []byte) (*MsgSplitter, error) {
	if len(initData) < 56 {
		return nil, fmt.Errorf("init too short")
	}
	block, err := aes.NewCipher(initData[8:40])
	if err != nil {
		return nil, err
	}
	s := cipher.NewCTR(block, initData[40:56])
	skip := make([]byte, 64)
	s.XORKeyStream(skip, skip)
	return &MsgSplitter{stream: s}, nil
}

func (s *MsgSplitter) Split(chunk []byte) [][]byte {
	plain := make([]byte, len(chunk))
	s.stream.XORKeyStream(plain, chunk)

	var bounds []int
	pos, n := 0, len(plain)
	for pos < n {
		var msgLen int
		if plain[pos] == 0x7f {
			if pos+4 > n {
				break
			}
			b := [4]byte{}
			copy(b[:3], plain[pos+1:pos+4])
			msgLen = int(binary.LittleEndian.Uint32(b[:])&0xFFFFFF) * 4
			pos += 4
		} else {
			msgLen = int(plain[pos]) * 4
			pos++
		}
		if msgLen == 0 || pos+msgLen > n {
			break
		}
		pos += msgLen
		bounds = append(bounds, pos)
	}
	if len(bounds) <= 1 {
		return [][]byte{chunk}
	}
	parts := make([][]byte, 0, len(bounds)+1)
	prev := 0
	for _, b := range bounds {
		parts = append(parts, chunk[prev:b])
		prev = b
	}
	if prev < len(chunk) {
		parts = append(parts, chunk[prev:])
	}
	return parts
}

func WsDomains(dc int, isMedia bool) []string {
	mapped := dc
	if o, ok := DCOverrides[dc]; ok {
		mapped = o
	}
	if isMedia {
		return []string{
			fmt.Sprintf("kws%d-1.web.telegram.org", mapped),
			fmt.Sprintf("kws%d.web.telegram.org", mapped),
		}
	}
	return []string{
		fmt.Sprintf("kws%d.web.telegram.org", mapped),
		fmt.Sprintf("kws%d-1.web.telegram.org", mapped),
	}
}

func HumanBytes(n int64) string {
	f := float64(n)
	for _, u := range []string{"B", "KB", "MB", "GB"} {
		if math.Abs(f) < 1024 {
			return fmt.Sprintf("%.1f%s", f, u)
		}
		f /= 1024
	}
	return fmt.Sprintf("%.1f TB", f)
}

func MediaTag(isMedia bool) string {
	if isMedia {
		return "m"
	}
	return ""
}
