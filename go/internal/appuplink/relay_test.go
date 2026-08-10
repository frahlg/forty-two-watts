package appuplink

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// fakeRelay is the routing half of srcfl/ftw-webapp relay/src/server.ts: one
// box uplink and up to four browser streams meet under a handle, binary
// messages cross between them, and the relay says two words.
//
// It is a fake rather than the real server because the real one is TypeScript;
// the parts that matter to this package — the join path, the two control
// words and the close codes — are copied, not invented, and
// relay_contract_test.go pins them to the constants the app compiled against.
type fakeRelay struct {
	server *httptest.Server

	mu      sync.Mutex
	epoch   int64
	uplink  *websocket.Conn
	streams []*websocket.Conn
	handles []string
	// rotateTo, when set, closes the next accepted uplink with 4410 and this
	// epoch, which is what the relay does when the hour turns.
	rotateTo *int64
	// refuseEpoch, when set, closes with 4409 and this epoch, which is what
	// the relay does when a box guessed wrong.
	refuseEpoch *int64

	// The dead man's switch half of the contract: rows keyed on id, and
	// the claims heard on the uplink socket, in order.
	deadmanRows   map[string]deadmanRowWire
	deadmanClaims []string

	joined  chan struct{}
	deadman chan struct{}
}

// deadmanRowWire is the POST /deadman body, as the contract writes it.
type deadmanRowWire struct {
	ID        string `json:"id"`
	Endpoint  string `json:"endpoint"`
	CT        string `json:"ct"`
	Auth      string `json:"auth"`
	DeadlineS int    `json:"deadline_s"`
}

func newFakeRelay(epoch int64) *fakeRelay {
	r := &fakeRelay{
		epoch:       epoch,
		joined:      make(chan struct{}, 16),
		deadman:     make(chan struct{}, 64),
		deadmanRows: map[string]deadmanRowWire{},
	}
	r.server = httptest.NewServer(http.HandlerFunc(r.handle))
	return r
}

func (r *fakeRelay) url() string {
	return "ws" + strings.TrimPrefix(r.server.URL, "http")
}

func (r *fakeRelay) close() { r.server.Close() }

func (r *fakeRelay) handle(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/deadman" || strings.HasPrefix(req.URL.Path, "/deadman/") {
		r.handleDeadman(w, req)
		return
	}
	parts := strings.Split(strings.TrimPrefix(req.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[0] != "r" {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	epoch, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		http.Error(w, "join", http.StatusBadRequest)
		return
	}
	handle, role := parts[2], parts[3]

	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		return
	}

	r.mu.Lock()
	r.handles = append(r.handles, handle)
	refuse, rotate, want := r.refuseEpoch, r.rotateTo, r.epoch
	r.mu.Unlock()

	select {
	case r.joined <- struct{}{}:
	default:
	}

	switch {
	case refuse != nil:
		r.mu.Lock()
		r.refuseEpoch = nil
		r.mu.Unlock()
		closeWith(conn, CloseEpoch, strconv.FormatInt(*refuse, 10))
		return
	case rotate != nil:
		r.mu.Lock()
		r.rotateTo = nil
		r.epoch = *rotate
		r.mu.Unlock()
		closeWith(conn, CloseRotated, strconv.FormatInt(*rotate, 10))
		return
	case epoch != want:
		closeWith(conn, CloseEpoch, strconv.FormatInt(want, 10))
		return
	}

	if role == "box" {
		r.serveUplink(conn)
		return
	}
	r.serveStream(conn)
}

func (r *fakeRelay) serveUplink(conn *websocket.Conn) {
	r.mu.Lock()
	r.uplink = conn
	streams := append([]*websocket.Conn(nil), r.streams...)
	r.mu.Unlock()

	// The relay's only two words. Both sides hear them, which is how a peer
	// knows there is anybody to talk to.
	for _, s := range streams {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
		_ = s.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
	}

	defer func() {
		r.mu.Lock()
		if r.uplink == conn {
			r.uplink = nil
		}
		r.mu.Unlock()
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			// The deadman-aware relay accepts exactly one text word from
			// the box: "deadman <id>" claims the id on this socket.
			// Anything else remains a protocol breach and drops the
			// connection, as before.
			word := string(message)
			if kind == websocket.TextMessage && strings.HasPrefix(word, CtrlDeadman+" ") {
				r.mu.Lock()
				r.deadmanClaims = append(r.deadmanClaims, strings.TrimPrefix(word, CtrlDeadman+" "))
				r.mu.Unlock()
				select {
				case r.deadman <- struct{}{}:
				default:
				}
				continue
			}
			return
		}
		// Broadcast, because the relay cannot tell which stream a ciphertext
		// belongs to without reading it.
		r.mu.Lock()
		streams := append([]*websocket.Conn(nil), r.streams...)
		r.mu.Unlock()
		for _, s := range streams {
			_ = s.WriteMessage(websocket.BinaryMessage, message)
		}
	}
}

func (r *fakeRelay) serveStream(conn *websocket.Conn) {
	r.mu.Lock()
	r.streams = append(r.streams, conn)
	uplink := r.uplink
	r.mu.Unlock()

	if uplink != nil {
		_ = uplink.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
		_ = conn.WriteMessage(websocket.TextMessage, []byte(CtrlReady))
	}

	defer func() {
		r.mu.Lock()
		for i, s := range r.streams {
			if s == conn {
				r.streams = append(r.streams[:i], r.streams[i+1:]...)
				break
			}
		}
		empty := len(r.streams) == 0
		up := r.uplink
		r.mu.Unlock()
		if empty && up != nil {
			_ = up.WriteMessage(websocket.TextMessage, []byte(CtrlGone))
		}
		_ = conn.Close()
	}()

	for {
		kind, message, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if kind != websocket.BinaryMessage {
			return
		}
		r.mu.Lock()
		up := r.uplink
		r.mu.Unlock()
		if up != nil {
			_ = up.WriteMessage(websocket.BinaryMessage, message)
		}
	}
}

func (r *fakeRelay) seenHandles() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.handles...)
}

// handleDeadman is the relay's HTTP half of the dead man's switch contract:
// POST /deadman upserts a row on id, DELETE /deadman/<id> forgets one,
// idempotently. 204 both ways. A row without a VAPID auth header is refused
// the way the real relay refuses it: the subscription is bound to the box's
// key, so a row the relay could never deliver must not arm the switch.
func (r *fakeRelay) handleDeadman(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodPost:
		var row deadmanRowWire
		if err := json.NewDecoder(req.Body).Decode(&row); err != nil ||
			len(row.ID) != 32 || row.Endpoint == "" || row.CT == "" ||
			!strings.HasPrefix(row.Auth, "vapid t=") ||
			row.DeadlineS < 60 || row.DeadlineS > 86400 {
			http.Error(w, "bad row", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		r.deadmanRows[row.ID] = row
		r.mu.Unlock()
		select {
		case r.deadman <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodDelete:
		id := strings.TrimPrefix(req.URL.Path, "/deadman/")
		r.mu.Lock()
		delete(r.deadmanRows, id)
		r.mu.Unlock()
		select {
		case r.deadman <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func (r *fakeRelay) deadmanState() (rows map[string]deadmanRowWire, claims []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows = make(map[string]deadmanRowWire, len(r.deadmanRows))
	for id, row := range r.deadmanRows {
		rows[id] = row
	}
	return rows, append([]string(nil), r.deadmanClaims...)
}

func closeWith(conn *websocket.Conn, code int, reason string) {
	_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(code, reason))
	_ = conn.Close()
}
