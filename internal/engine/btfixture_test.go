package engine

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// btFixture is an offline BitTorrent swarm for wrapper tests: an HTTP
// tracker and a seed peer that serves metadata over ut_metadata and
// payload pieces. It uses no aria2go internals.
type btFixture struct {
	TorrentData []byte
	InfoRaw     []byte
	InfoHash    [20]byte
	Name        string

	payload []byte
	piece   int
	peerLn  net.Listener
	tracker *httptest.Server
	cancel  context.CancelFunc
}

func startBTFixture(t *testing.T, name string, payload []byte, pieceLength int) *btFixture {
	t.Helper()

	peerLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bt peer listen: %v", err)
	}
	peerPort := peerLn.Addr().(*net.TCPAddr).Port

	tracker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/announce" {
			http.NotFound(w, r)
			return
		}
		ip := net.ParseIP("127.0.0.1").To4()
		var compact [6]byte
		copy(compact[:4], ip)
		binary.BigEndian.PutUint16(compact[4:], uint16(peerPort))
		resp := bencodeDict(
			bencodeBytes([]byte("interval")), bencodeInt(1800),
			bencodeBytes([]byte("peers")), bencodeBytes(compact[:]),
		)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(resp)
	}))

	torrentData, infoRaw, infoHash := buildTorrent(tracker.URL+"/announce", name, payload, pieceLength)

	ctx, cancel := context.WithCancel(context.Background())
	f := &btFixture{
		TorrentData: torrentData,
		InfoRaw:     infoRaw,
		InfoHash:    infoHash,
		Name:        name,
		payload:     payload,
		piece:       pieceLength,
		peerLn:      peerLn,
		tracker:     tracker,
		cancel:      cancel,
	}
	go f.servePeer(ctx)
	t.Cleanup(f.Close)
	return f
}

func (f *btFixture) Close() {
	f.cancel()
	_ = f.peerLn.Close()
	f.tracker.Close()
}

// MagnetURI returns a magnet link for the fixture swarm.
func (f *btFixture) MagnetURI() string {
	return fmt.Sprintf(
		"magnet:?xt=urn:btih:%s&dn=%s&tr=%s",
		hex.EncodeToString(f.InfoHash[:]),
		f.Name,
		f.tracker.URL+"/announce",
	)
}

func buildTorrent(announce, name string, data []byte, pieceLength int) ([]byte, []byte, [20]byte) {
	var pieces []byte
	for off := 0; off < len(data); off += pieceLength {
		end := off + pieceLength
		if end > len(data) {
			end = len(data)
		}
		sum := sha1.Sum(data[off:end])
		pieces = append(pieces, sum[:]...)
	}
	info := bencodeDict(
		bencodeBytes([]byte("length")), bencodeInt(int64(len(data))),
		bencodeBytes([]byte("name")), bencodeBytes([]byte(name)),
		bencodeBytes([]byte("piece length")), bencodeInt(int64(pieceLength)),
		bencodeBytes([]byte("pieces")), bencodeBytes(pieces),
	)
	top := bencodeDict(
		bencodeBytes([]byte("announce")), bencodeBytes([]byte(announce)),
		// A nested dictionary value is embedded raw, not length-prefixed.
		bencodeBytes([]byte("info")), info,
	)
	return top, info, sha1.Sum(info)
}

// Minimal bencode encoders, enough for fixture payloads.

func bencodeBytes(b []byte) []byte {
	return []byte(fmt.Sprintf("%d:", len(b)) + string(b))
}

func bencodeInt(i int64) []byte {
	return []byte(fmt.Sprintf("i%de", i))
}

func bencodeDict(pairs ...[]byte) []byte {
	var out []byte
	out = append(out, 'd')
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, pairs[i]...)
		out = append(out, pairs[i+1]...)
	}
	out = append(out, 'e')
	return out
}

// bencodeFindInt extracts the value of key from a bencoded dictionary body.
func bencodeFindInt(dict []byte, key string) (int64, bool) {
	needle := []byte(fmt.Sprintf("%d:%s", len(key), key))
	idx := bytes.Index(dict, needle)
	if idx < 0 {
		return 0, false
	}
	rest := dict[idx+len(needle):]
	if len(rest) == 0 || rest[0] != 'i' {
		return 0, false
	}
	end := bytes.IndexByte(rest, 'e')
	if end < 0 {
		return 0, false
	}
	var v int64
	if _, err := fmt.Sscanf(string(rest[1:end]), "%d", &v); err != nil {
		return 0, false
	}
	return v, true
}

func (f *btFixture) servePeer(ctx context.Context) {
	for {
		conn, err := f.peerLn.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		go f.handlePeer(ctx, conn)
	}
}

func (f *btFixture) handlePeer(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var hs [68]byte
	if _, err := io.ReadFull(conn, hs[:]); err != nil {
		return
	}
	if hs[0] != 19 || string(hs[1:20]) != "BitTorrent protocol" {
		return
	}
	if !bytes.Equal(hs[28:48], f.InfoHash[:]) {
		return
	}

	var resp [68]byte
	resp[0] = 19
	copy(resp[1:20], "BitTorrent protocol")
	resp[20+5] = 0x10 // extension protocol (BEP 10) for ut_metadata
	copy(resp[28:48], f.InfoHash[:])
	copy(resp[48:68], []byte("-TM0001-fixturepeer"))
	if _, err := conn.Write(resp[:]); err != nil {
		return
	}

	// Extended handshake: message id 20 (extended), payload starts with
	// extended-handshake id 0, then the bencoded dictionary.
	extHandshake := bencodeDict(
		bencodeBytes([]byte("m")), bencodeDict(
			bencodeBytes([]byte("ut_metadata")), bencodeInt(3),
		),
		bencodeBytes([]byte("metadata_size")), bencodeInt(int64(len(f.InfoRaw))),
	)
	if _, err := conn.Write(peerMessage(20, append([]byte{0}, extHandshake...))); err != nil {
		return
	}

	var (
		infoRaw          = f.InfoRaw
		bitfieldSent     bool
		clientMetadataID uint8
	)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var lenBuf [4]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		msgLen := binary.BigEndian.Uint32(lenBuf[:])
		if msgLen == 0 {
			continue
		}
		payload := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		if len(payload) == 0 {
			continue
		}

		switch payload[0] {
		case 5, 2: // bitfield, interested
			if bitfieldSent {
				continue
			}
			// unchoke
			if _, err := conn.Write([]byte{0, 0, 0, 1, 1}); err != nil {
				return
			}
			if err := f.writeBitfield(conn); err != nil {
				return
			}
			bitfieldSent = true
		case 20: // extended
			if len(payload) < 2 {
				continue
			}
			if payload[1] == 0 {
				// Extended handshake: find our ut_metadata id, "3:<id>".
				if id, ok := bencodeFindInt(payload[2:], "ut_metadata"); ok && id > 0 && id < 256 {
					clientMetadataID = uint8(id)
				}
				continue
			}
			if payload[1] != 3 || clientMetadataID == 0 {
				continue
			}
			piece, ok := bencodeFindInt(payload[2:], "piece")
			if !ok {
				return
			}
			if err := f.writeMetadataPiece(conn, clientMetadataID, int(piece), infoRaw); err != nil {
				return
			}
		case 6: // request
			if len(payload) < 13 {
				return
			}
			index := binary.BigEndian.Uint32(payload[1:5])
			begin := binary.BigEndian.Uint32(payload[5:9])
			length := binary.BigEndian.Uint32(payload[9:13])
			if err := f.writePiece(conn, index, begin, length); err != nil {
				return
			}
		}
	}
}

func peerMessage(id byte, payload []byte) []byte {
	msg := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(msg[:4], uint32(1+len(payload)))
	msg[4] = id
	copy(msg[5:], payload)
	return msg
}

func (f *btFixture) writeMetadataPiece(w io.Writer, extID uint8, piece int, infoRaw []byte) error {
	const metadataPieceSize = 16384
	start := piece * metadataPieceSize
	if start < 0 || start >= len(infoRaw) {
		return nil
	}
	end := start + metadataPieceSize
	if end > len(infoRaw) {
		end = len(infoRaw)
	}
	dict := bencodeDict(
		bencodeBytes([]byte("msg_type")), bencodeInt(1),
		bencodeBytes([]byte("piece")), bencodeInt(int64(piece)),
		bencodeBytes([]byte("total_size")), bencodeInt(int64(len(infoRaw))),
	)
	body := append(dict, infoRaw[start:end]...)
	_, err := w.Write(peerMessage(20, append([]byte{extID}, body...)))
	return err
}

func (f *btFixture) writeBitfield(w io.Writer) error {
	numPieces := (len(f.payload) + f.piece - 1) / f.piece
	bitfield := make([]byte, (numPieces+7)/8)
	for i := 0; i < numPieces; i++ {
		bitfield[i/8] |= 1 << (7 - (i % 8))
	}
	_, err := w.Write(peerMessage(5, bitfield))
	return err
}

func (f *btFixture) writePiece(w io.Writer, index, begin, length uint32) error {
	offset := int64(index)*int64(f.piece) + int64(begin)
	if offset < 0 || offset >= int64(len(f.payload)) {
		return nil
	}
	end := offset + int64(length)
	if end > int64(len(f.payload)) {
		end = int64(len(f.payload))
	}
	block := f.payload[offset:end]
	msg := make([]byte, 4+1+8+len(block))
	binary.BigEndian.PutUint32(msg[:4], uint32(9+len(block)))
	msg[4] = 7
	binary.BigEndian.PutUint32(msg[5:9], index)
	binary.BigEndian.PutUint32(msg[9:13], begin)
	copy(msg[13:], block)
	_, err := w.Write(msg)
	return err
}

// harness helpers for the wrapper tests.

func startEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := New(cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- e.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Errorf("Run did not return after context cancel")
		}
	})
	return e
}

func waitForEvent(t *testing.T, events <-chan Event, gid string, typ EventType, timeout time.Duration) Event {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-events:
			if ev.GID == gid && ev.Type == typ {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s event for GID %s", typ, gid)
			return Event{}
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func writeCAFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	var pemOut bytes.Buffer
	if err := pem.Encode(&pemOut, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}); err != nil {
		t.Fatalf("encode CA PEM: %v", err)
	}
	if err := os.WriteFile(caPath, pemOut.Bytes(), 0o600); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	return caPath
}
