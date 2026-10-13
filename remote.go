// Acesso remoto à tela (Fase 3) — WebRTC via data channel.
// O navegador do técnico (Hub360) é o "offerer": cria a conexão e os canais
// "frames" (recebe a tela) e "input" (envia mouse/teclado). O agente responde,
// captura a tela e injeta os comandos de mouse/teclado.
// Sinalização passa pelo canal da Cloudflare (/agent/signal).

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/jpeg"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
)

const (
	frameFPS     = 10
	jpegQuality  = 40
	chunkSize    = 60000 // bytes por pedaço no data channel
	frameMarker  = 0xF1
)

var activeSessions sync.Map // sessionID -> *webrtc.PeerConnection

// Chamado quando o heartbeat avisa que há uma sessão pendente.
func startRemoteSession(cfg *Config, sessionID string) {
	if _, loaded := activeSessions.LoadOrStore(sessionID, true); loaded {
		return // já em andamento
	}
	defer activeSessions.Delete(sessionID)

	api := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
	}
	pc, err := webrtc.NewPeerConnection(api)
	if err != nil {
		return
	}
	defer pc.Close()

	stop := make(chan struct{})
	var stopOnce sync.Once
	closeAll := func() { stopOnce.Do(func() { close(stop) }) }

	// Envia nossos candidatos ICE para o técnico
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		ci := c.ToJSON()
		b, _ := json.Marshal(ci)
		postJSON("/agent/signal", map[string]any{
			"token": cfg.Token, "session_id": sessionID, "candidate": string(b),
		})
	})

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateDisconnected {
			closeAll()
		}
	})

	// Canais criados pelo técnico
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "frames":
			dc.OnOpen(func() { go streamFrames(dc, stop) })
		case "input":
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				handleInput(msg.Data)
			})
		}
	})

	// 1) Busca a oferta do técnico
	var offer string
	for i := 0; i < 40; i++ { // ~20s de espera
		resp, _ := postJSON("/agent/signal", map[string]any{"token": cfg.Token, "session_id": sessionID})
		if resp != nil {
			if o, ok := resp["offer_sdp"].(string); ok && o != "" {
				offer = o
				break
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if offer == "" {
		return
	}

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		return
	}
	postJSON("/agent/signal", map[string]any{
		"token": cfg.Token, "session_id": sessionID, "answer_sdp": answer.SDP,
	})

	// 2) Continua buscando candidatos do técnico
	go func() {
		seen := map[string]bool{}
		for {
			select {
			case <-stop:
				return
			case <-time.After(700 * time.Millisecond):
			}
			resp, _ := postJSON("/agent/signal", map[string]any{"token": cfg.Token, "session_id": sessionID})
			if resp == nil {
				continue
			}
			if arr, ok := resp["tech_candidates"].([]any); ok {
				for _, raw := range arr {
					s, _ := raw.(string)
					if s == "" || seen[s] {
						continue
					}
					seen[s] = true
					var ci webrtc.ICECandidateInit
					if json.Unmarshal([]byte(s), &ci) == nil {
						pc.AddICECandidate(ci)
					}
				}
			}
		}
	}()

	// Mantém a sessão viva até cair (timeout de segurança: 2h)
	select {
	case <-stop:
	case <-time.After(2 * time.Hour):
	}
}

// Captura a tela e envia em pedaços pelo data channel "frames".
func streamFrames(dc *webrtc.DataChannel, stop chan struct{}) {
	ticker := time.NewTicker(time.Second / frameFPS)
	defer ticker.Stop()
	var frameID uint32

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}
		img := captureFrame(currentMonitor())
		if img == nil {
			continue
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
			continue
		}
		data := buf.Bytes()
		frameID++
		sendFrame(dc, frameID, data)
	}
}

func sendFrame(dc *webrtc.DataChannel, frameID uint32, data []byte) {
	total := (len(data) + chunkSize - 1) / chunkSize
	if total == 0 {
		total = 1
	}
	for i := 0; i < total; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		header := make([]byte, 9)
		header[0] = frameMarker
		binary.LittleEndian.PutUint32(header[1:], frameID)
		binary.LittleEndian.PutUint16(header[5:], uint16(i))
		binary.LittleEndian.PutUint16(header[7:], uint16(total))
		packet := append(header, data[start:end]...)
		if err := dc.Send(packet); err != nil {
			return
		}
	}
}

// handleInput recebe eventos do navegador (JSON) e injeta no Windows.
type inputEvent struct {
	T      string  `json:"t"`      // move, down, up, scroll, key, text
	X      float64 `json:"x"`      // normalizado 0..1
	Y      float64 `json:"y"`      // normalizado 0..1
	Button string  `json:"button"` // left, right, middle
	Delta  float64 `json:"delta"`  // scroll
	Key    string  `json:"key"`    // tecla
	Code   string  `json:"code"`   // código físico
	Down   bool    `json:"down"`   // keydown/keyup
	Ctrl   bool    `json:"ctrl"`
	Alt    bool    `json:"alt"`
	Shift  bool    `json:"shift"`
	Text   string  `json:"text"`
	Monitor int    `json:"monitor"` // troca de monitor (t="monitor")
}

func handleInput(data []byte) {
	var ev inputEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	injectInput(ev)
}

// image.Image auxiliar para evitar import não usado em builds sem captura.
var _ = image.RGBA{}
