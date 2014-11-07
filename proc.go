package procspy

// /proc based implementation

import (
	"bytes"
	"net"
	"os"
	"strconv"
	"syscall"
)

const (
	procRoot = "/proc"
)

// procProcesses gives all processes for the given connections. It is used by
// the linux version of Processes().
func procProcesses(conn []Connection) []ConnectionProc {
	// A map of inode -> pid
	inodes, err := walkProcPid()
	if err != nil {
		return nil
	}

	res := []ConnectionProc{}
	for _, tp := range conn {
		if pid, ok := inodes[tp.inode]; ok {
			name, err := procName(pid)
			if err != nil {
				// Process might be gone by now
				continue
			}
			// conn only has 'Established' connections.
			// if net.IP(tp.RemoteAddress).IsUnspecified() {
			// // Remote address is zero. This is a listen entry.
			// continue
			// }
			res = append(res, ConnectionProc{
				// Connection: tp,
				Connection: Connection{
					Transport:     tp.Transport,
					LocalAddress:  tp.LocalAddress,
					LocalPort:     tp.LocalPort,
					RemoteAddress: tp.RemoteAddress,
					RemotePort:    tp.RemotePort,
					inode:         tp.inode,
				},
				PID:  pid,
				Name: name,
			})
		}
	}
	return res
}

func walkProcPid() (map[uint64]uint, error) {
	// Walk over all /proc entries (numerical ones, those are PIDs), and see if
	// their ./fd/* files are symlink to sockets.
	// Returns a map from socket id ('inode`) to PID.
	// Will return an error if /proc/ isn't there.
	fh, err := os.Open(procRoot)
	if err != nil {
		return nil, err
	}
	dirNames, err := fh.Readdirnames(-1)
	fh.Close()
	if err != nil {
		return nil, err
	}
	procmap := map[uint64]uint{}
	var stat syscall.Stat_t
	for _, dirName := range dirNames {
		pid, err := strconv.ParseUint(dirName, 10, 0)
		if err != nil {
			// Not a number, so not a PID subdir.
			continue
		}

		fdBase := procRoot + "/" + dirName + "/fd/"
		dfh, err := os.Open(fdBase)
		if err != nil {
			// Process is be gone by now, or we don't have access.
			continue
		}
		fdNames, err := dfh.Readdirnames(-1)
		dfh.Close()
		if err != nil {
			continue
		}
		for _, fdName := range fdNames {
			// Direct use of syscall.Stat() to save garbage.
			err = syscall.Stat(fdBase+fdName, &stat)
			if err != nil {
				continue
			}
			// We want sockets only
			if stat.Mode&syscall.S_IFMT != syscall.S_IFSOCK {
				continue
			}
			procmap[stat.Ino] = uint(pid)
		}
	}
	return procmap, nil
}

// parseTransport parses /proc/net/{tcp,udp}{,6} files.
// It will filter out all rows not in wantedState.
func parseTransport(s []byte, wantedState uint) []Connection {
	// The file format is well-known, so we use some specialized versions of
	// std lib functions to speed things up a bit.

	res := make([]Connection, 0, len(s)/149) // heuristic

	// Skip header
	cursor := bytes.IndexByte(s, '\n')
	if cursor == -1 {
		return nil
	}
	s = s[cursor+1:]

	// Reuse fields every line. We know there are 21 fields in a line, but we
	// don't need the last few.
	fields := [18][]byte{}
	for {
		cursor = bytes.IndexByte(s, '\n')
		if cursor == -1 {
			break
		}
		line := s[:cursor]
		s = s[cursor+1:]
		if len(line) == 0 {
			break
		}
		// Fields are split on ' ' and ':'(!):
		// '  sl local_address rem_address st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode <more>'
		// '  0: 00000000:0FC9 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11276449 1 ffff8801029607c0 100 0 0 10 0'
		procNetFields(line, &fields)

		if state := parseHex(fields[5]); state != wantedState {
			continue
		}

		t := Connection{}

		t.LocalAddress = net.IP(scanAddress(fields[1]))
		t.LocalPort = uint16(parseHex(fields[2]))
		t.RemoteAddress = net.IP(scanAddress(fields[3]))
		t.RemotePort = uint16(parseHex(fields[4]))
		t.inode = parseDec(fields[13])

		res = append(res, t)
	}
	return res
}

// scanAddress parses 'A12CF62E' to the address.
// Handles IPv4 and IPv6 addresses.
// The address is a big endian 32 bit ints, hex encoded. Since net.IP is a
// byte slice we just decode the hex and flip the bytes in every group of 4.
func scanAddress(in []byte) []byte {
	// Network address is big endian. Can be either ipv4 or ipv6.
	address := hexDecode(in)
	// reverse every 4 byte-sequence.
	for i := 0; i < len(address); i += 4 {
		address[i], address[i+3] = address[i+3], address[i]
		address[i+1], address[i+2] = address[i+2], address[i+1]
	}
	return address
}

// procName does a pid->name lookup
func procName(pid uint) (string, error) {
	fh, err := os.Open(procRoot + "/" + strconv.FormatUint(uint64(pid), 10) + "/comm")
	if err != nil {
		return "", err
	}
	name := make([]byte, 1024)
	l, err := fh.Read(name)
	fh.Close()
	if err != nil {
		return "", err
	}
	if l < 2 {
		return "", nil
	}
	// drop trailing "\n"
	return string(name[:l-1]), nil
}

// Copy of the standard strings.FieldsFunc(), but just for our tcp lines.
// We split on ' ' and ':'.
func procNetFields(s []byte, a *[18][]byte) {
	na := 0
	fieldStart := -1 // Set to -1 when looking for start of field.
	for i := 0; i < len(s) && na < len(*a); i++ {
		switch s[i] {
		case ' ', ':':
			if fieldStart >= 0 {
				(*a)[na] = s[fieldStart:i]
				na++
				fieldStart = -1
			}
		default:
			if fieldStart == -1 {
				fieldStart = i
			}
		}
	}
}

// readFile reads a /proc file info a buffer.
func readFile(filename string, buf *bytes.Buffer) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	_, err = buf.ReadFrom(f)
	f.Close()
	return err
}

// Simplified copy of strconv.ParseUint(16).
func parseHex(s []byte) uint {
	n := uint(0)
	for i := 0; i < len(s); i++ {
		n *= 16
		n += uint(fromHexChar(s[i]))
	}
	return n
}

// Simplified copy of strconv.ParseUint(10).
func parseDec(s []byte) uint64 {
	n := uint64(0)
	for _, c := range s {
		n *= 10
		n += uint64(c - '0')
	}
	return n
}

// hexDecode and fromHexChar are taken from encoding/hex.
func hexDecode(src []byte) []byte {
	if len(src)%2 == 1 {
		return nil
	}

	dst := make([]byte, len(src)/2)
	for i := 0; i < len(src)/2; i++ {
		a := fromHexChar(src[i*2])
		b := fromHexChar(src[i*2+1])
		dst[i] = (a << 4) | b
	}

	return dst
}

// fromHexChar converts a hex character into its value.
func fromHexChar(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}

	return 0
}
