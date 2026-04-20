package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

//go:embed static
var staticFS embed.FS

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var palette = []string{
	"#e74c3c", "#3498db", "#2ecc71", "#f39c12",
	"#9b59b6", "#1abc9c", "#e67e22", "#e91e63",
	"#00bcd4", "#8bc34a", "#ff5722", "#607d8b",
}

var wordBank = []string{
	"apple", "baby", "train", "zebra", "house", "clock", "flower",
	"dragon", "guitar", "rocket", "penguin", "umbrella", "castle",
	"robot", "pizza", "volcano", "butterfly", "telescope", "lighthouse",
	"sandwich", "cactus", "submarine", "rainbow", "pyramid", "tornado",
	"elephant", "spaceship", "bicycle", "waterfall", "snowman",
	"treasure", "mushroom", "lantern", "compass", "anchor", "crown",
	"balloon", "cannon", "dinosaur", "fireworks", "astrolabe", "sextant",
	"obelisk", "thimble", "retort", "sundial", "love", "happiness",
	"kindness", "curiosity", "friendship", "wonder",
}

const (
	maxOps      = 100_000
	turnSeconds = 120
	betweenSecs = 5
)

// ── Wire types ─────────────────────────────────────────────────────────────

type Op struct {
	Type  string  `json:"type"`
	X0    float64 `json:"x0"`
	Y0    float64 `json:"y0"`
	X1    float64 `json:"x1"`
	Y1    float64 `json:"y1"`
	Color string  `json:"color"`
	Size  float64 `json:"size"`
}

type Player struct {
	Name  string `json:"name"`
	Color string `json:"color"`
	Score int    `json:"score"`
}

type inMsg struct {
	Type string  `json:"type"` // join | line | erase | guess
	Name string  `json:"name,omitempty"`
	X0   float64 `json:"x0,omitempty"`
	Y0   float64 `json:"y0,omitempty"`
	X1   float64 `json:"x1,omitempty"`
	Y1   float64 `json:"y1,omitempty"`
	Size float64 `json:"size,omitempty"`
	Text string  `json:"text,omitempty"`
}

type outMsg struct {
	Type     string   `json:"type"`
	Canvas   []Op     `json:"canvas,omitempty"`
	Op       *Op      `json:"op,omitempty"`
	Players  []Player `json:"players,omitempty"`
	Color    string   `json:"color,omitempty"`
	Err      string   `json:"err,omitempty"`
	// turn state
	Role     string `json:"role,omitempty"`    // draw | guess | watch
	Drawer   string `json:"drawer,omitempty"`
	Word     string `json:"word,omitempty"`    // only sent to the drawer
	WordLen  int    `json:"wordLen,omitempty"` // sent to everyone else
	TimeLeft int    `json:"timeLeft,omitempty"`
	Clear    bool   `json:"clear,omitempty"`
	// chat / guess events
	Sender  string `json:"sender,omitempty"`
	Text    string `json:"text,omitempty"`
	Correct bool   `json:"correct,omitempty"`
	// turn end
	Reason  string `json:"reason,omitempty"` // timeout | guessed | left
	Guesser string `json:"guesser,omitempty"`
}

// ── Client / Hub ───────────────────────────────────────────────────────────

var nextID atomic.Uint64

type client struct {
	hub     *Hub
	conn    *websocket.Conn
	send    chan []byte
	id      uint64
	name    string
	color   string
	score   int
	guessed bool // already guessed correctly this turn
}

type Hub struct {
	clients  map[uint64]*client
	canvas   []Op
	colorIdx int
	actions  chan func()

	// game state
	phase           string   // "waiting" | "drawing"
	drawerID        uint64
	word            string
	turnStart       time.Time
	turnTimer       *time.Timer
	drawOrder       []uint64 // joined player IDs, round-robin
	drawerPos       int
	forcedNextDrawer uint64  // if nonzero, next turn's drawer
	lastGuesser     string
	words           []string
	wordIdx         int
}

func newHub() *Hub {
	words := make([]string, len(wordBank))
	copy(words, wordBank)
	rand.Shuffle(len(words), func(i, j int) { words[i], words[j] = words[j], words[i] })

	h := &Hub{
		clients: make(map[uint64]*client),
		actions: make(chan func(), 512),
		phase:   "waiting",
		words:   words,
	}
	go func() {
		for fn := range h.actions {
			fn()
		}
	}()
	return h
}

func (h *Hub) do(fn func()) { h.actions <- fn }

// ── Player list ────────────────────────────────────────────────────────────

func (h *Hub) players() []Player {
	var out []Player
	for _, c := range h.clients {
		if c.name != "" {
			out = append(out, Player{c.name, c.color, c.score})
		}
	}
	return out
}

// ── Send helpers ───────────────────────────────────────────────────────────

func (h *Hub) sendTo(c *client, msg outMsg) {
	data, _ := json.Marshal(msg)
	select {
	case c.send <- data:
	default:
		h.remove(c)
	}
}

// broadcast sends msg to all connected clients.
// It is safe to call remove (which modifies h.clients) during the range —
// Go skips map keys deleted mid-iteration.
func (h *Hub) broadcast(msg outMsg) {
	data, _ := json.Marshal(msg)
	for _, c := range h.clients {
		select {
		case c.send <- data:
		default:
			h.remove(c)
		}
	}
}

func (h *Hub) remove(c *client) {
	if _, ok := h.clients[c.id]; !ok {
		return
	}
	delete(h.clients, c.id)
	for i, id := range h.drawOrder {
		if id == c.id {
			h.drawOrder = append(h.drawOrder[:i], h.drawOrder[i+1:]...)
			if h.drawerPos > i {
				h.drawerPos--
			}
			break
		}
	}
	close(c.send)
	if c.name != "" {
		h.broadcast(outMsg{Type: "players", Players: h.players()})
	}
	if h.phase == "drawing" && c.id == h.drawerID {
		h.endTurn("left")
	} else {
		h.maybeStart()
	}
}

// ── WebSocket serving ──────────────────────────────────────────────────────

func (h *Hub) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	c := &client{
		hub:  h,
		conn: conn,
		send: make(chan []byte, 256),
		id:   nextID.Add(1),
	}
	h.do(func() {
		h.clients[c.id] = c
		snap := make([]Op, len(h.canvas))
		copy(snap, h.canvas)
		msg := outMsg{Type: "welcome", Canvas: snap, Players: h.players()}
		if h.phase == "drawing" {
			if drawer, ok := h.clients[h.drawerID]; ok {
				msg.Role = "watch"
				msg.Drawer = drawer.name
				msg.WordLen = len(h.word)
				msg.TimeLeft = h.timeLeft()
			}
		}
		h.sendTo(c, msg)
	})
	go c.writePump()
	go c.readPump()
}

func (c *client) readPump() {
	defer func() {
		c.hub.do(func() { c.hub.remove(c) })
		c.conn.Close()
	}()
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			break
		}
		var m inMsg
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		c.hub.do(func() { c.hub.handle(c, m) })
	}
}

// ── Message dispatch ───────────────────────────────────────────────────────

func (h *Hub) handle(c *client, m inMsg) {
	switch m.Type {
	case "join":
		if m.Name == "" || c.name != "" {
			return
		}
		c.name = m.Name
		c.color = palette[h.colorIdx%len(palette)]
		h.colorIdx++
		h.drawOrder = append(h.drawOrder, c.id)
		players := h.players()
		h.sendTo(c, outMsg{Type: "joined", Color: c.color, Players: players})
		h.broadcast(outMsg{Type: "players", Players: players})
		if h.phase == "drawing" {
			if drawer, ok := h.clients[h.drawerID]; ok {
				h.sendTo(c, outMsg{
					Type:     "turn",
					Role:     "guess",
					Drawer:   drawer.name,
					WordLen:  len(h.word),
					TimeLeft: h.timeLeft(),
					Players:  players,
				})
			}
		}
		h.maybeStart()

	case "clear":
		if c.name == "" || h.phase != "drawing" {
			return
		}
		h.canvas = nil
		h.broadcast(outMsg{Type: "clear"})

	case "line", "erase":
		if c.name == "" || h.phase != "drawing" {
			return
		}
		size, color := m.Size, c.color
		if m.Type == "erase" {
			color = "#ffffff"
			if size < 10 {
				size = 20
			}
		} else if size < 1 {
			size = 4
		}
		op := Op{Type: m.Type, X0: m.X0, Y0: m.Y0, X1: m.X1, Y1: m.Y1, Color: color, Size: size}
		if len(h.canvas) < maxOps {
			h.canvas = append(h.canvas, op)
		}
		h.broadcast(outMsg{Type: "op", Op: &op})

	case "guess":
		if c.name == "" || h.phase != "drawing" || c.id == h.drawerID || c.guessed {
			return
		}
		text := strings.TrimSpace(m.Text)
		if text == "" {
			return
		}
		if strings.EqualFold(text, h.word) {
			c.guessed = true
			c.score++
			h.lastGuesser = c.name
			h.broadcast(outMsg{Type: "chat", Sender: c.name, Correct: true})
			h.forcedNextDrawer = c.id
			h.endTurn("guessed")
		} else {
			h.broadcast(outMsg{Type: "chat", Sender: c.name, Text: text})
		}
	}
}

// ── Game logic ─────────────────────────────────────────────────────────────

func (h *Hub) maybeStart() {
	if h.phase != "waiting" {
		return
	}
	named := 0
	for _, c := range h.clients {
		if c.name != "" {
			named++
		}
	}
	if named >= 2 {
		h.startTurn()
	}
}

func (h *Hub) startTurn() {
	if len(h.drawOrder) < 2 {
		h.phase = "waiting"
		return
	}

	// Pick the next drawer.
	var drawerID uint64
	if h.forcedNextDrawer != 0 {
		if c, ok := h.clients[h.forcedNextDrawer]; ok && c.name != "" {
			drawerID = h.forcedNextDrawer
			for i, id := range h.drawOrder {
				if id == h.forcedNextDrawer {
					h.drawerPos = i + 1
					break
				}
			}
		}
		h.forcedNextDrawer = 0
	}
	if drawerID == 0 {
		for range h.drawOrder {
			pos := h.drawerPos % len(h.drawOrder)
			id := h.drawOrder[pos]
			h.drawerPos++
			if c, ok := h.clients[id]; ok && c.name != "" {
				drawerID = c.id
				break
			}
		}
	}
	if drawerID == 0 {
		h.phase = "waiting"
		return
	}

	h.drawerID = drawerID
	h.word = h.words[h.wordIdx%len(h.words)]
	h.wordIdx++
	h.phase = "drawing"
	h.turnStart = time.Now()
	h.canvas = nil
	h.lastGuesser = ""

	for _, c := range h.clients {
		c.guessed = false
	}

	drawer := h.clients[h.drawerID]
	players := h.players()

	for _, c := range h.clients {
		msg := outMsg{
			Type:     "turn",
			Drawer:   drawer.name,
			TimeLeft: turnSeconds,
			Players:  players,
			Clear:    true,
		}
		switch {
		case c.id == h.drawerID:
			msg.Role = "draw"
			msg.Word = h.word
		case c.name != "":
			msg.Role = "guess"
			msg.WordLen = len(h.word)
		default:
			msg.Role = "watch"
			msg.WordLen = len(h.word)
		}
		h.sendTo(c, msg)
	}

	if h.turnTimer != nil {
		h.turnTimer.Stop()
	}
	h.turnTimer = time.AfterFunc(turnSeconds*time.Second, func() {
		h.do(func() { h.endTurn("timeout") })
	})
}

func (h *Hub) endTurn(reason string) {
	if h.phase != "drawing" {
		return
	}
	h.phase = "waiting"
	if h.turnTimer != nil {
		h.turnTimer.Stop()
		h.turnTimer = nil
	}
	h.broadcast(outMsg{
		Type:    "turn_end",
		Word:    h.word,
		Reason:  reason,
		Guesser: h.lastGuesser,
		Players: h.players(),
	})
	time.AfterFunc(betweenSecs*time.Second, func() {
		h.do(h.maybeStart)
	})
}

func (h *Hub) timeLeft() int {
	left := turnSeconds - int(time.Since(h.turnStart).Seconds())
	if left < 0 {
		return 0
	}
	return left
}

// ── Pumps ──────────────────────────────────────────────────────────────────

func (c *client) writePump() {
	defer c.conn.Close()
	for data := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
			break
		}
	}
}

func main() {
	hub := newHub()
	sub, _ := fs.Sub(staticFS, "static")
	http.Handle("/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/ws", hub.serveWS)
	log.Println("Listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
