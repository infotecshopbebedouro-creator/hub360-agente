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
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
	xdraw "golang.org/x/image/draw"
)

const (
	chunkSize     = 60000 // bytes por pedaço no data channel
	frameMarker   = 0xF1
	maxBufferedKB = 400 // se o canal tiver mais que isto na fila, pula o frame
)

// Parâmetros de qualidade ajustáveis em tempo real pelo técnico.
var (
	streamQuality int32 = 38   // qualidade JPEG (1-95)
	streamWidth   int32 = 1366 // largura máxima enviada
	streamFPS     int32 = 12   // quadros por segundo alvo
	streamMonitor int32        // monitor em transmissão
)

func applyQuality(ev inputEvent) {
	if ev.Quality > 0 {
		atomic.StoreInt32(&streamQuality, int32(clampInt(ev.Quality, 10, 90)))
	}
	if ev.Width > 0 {
		atomic.StoreInt32(&streamWidth, int32(clampInt(ev.Width, 640, 3840)))
	}
	if ev.Fps > 0 {
		atomic.StoreInt32(&streamFPS, int32(clampInt(ev.Fps, 3, 30)))
	}
	logf("[tela] qualidade ajustada: q=%d largura=%d fps=%d",
		atomic.LoadInt32(&streamQuality), atomic.LoadInt32(&streamWidth), atomic.LoadInt32(&streamFPS))
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Reduz a imagem se for mais larga que a largura escolhida, mantendo proporção.
func downscale(src image.Image) image.Image {
	mw := int(atomic.LoadInt32(&streamWidth))
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= mw {
		return src
	}
	nh := h * mw / w
	dst := image.NewRGBA(image.Rect(0, 0, mw, nh))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

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
	logf("[tela] iniciando sessão %s", sessionID)
	pc, err := webrtc.NewPeerConnection(api)
	if err != nil {
		logf("[tela] erro ao criar PeerConnection: %v", err)
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
		logf("[tela] estado da conexão: %s", s.String())
		if s == webrtc.PeerConnectionStateFailed || s == webrtc.PeerConnectionStateClosed || s == webrtc.PeerConnectionStateDisconnected {
			closeAll()
		}
	})
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		logf("[tela] estado ICE: %s", s.String())
	})

	// Canais criados pelo técnico
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		logf("[tela] canal recebido: %s", dc.Label())
		switch dc.Label() {
		case "frames":
			dc.OnOpen(func() { logf("[tela] canal frames aberto, iniciando captura"); go streamFrames(dc, stop) })
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
		logf("[tela] oferta do técnico não chegou (timeout)")
		return
	}
	logf("[tela] oferta recebida, respondendo…")

	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		logf("[tela] erro SetRemoteDescription: %v", err)
		return
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		logf("[tela] erro CreateAnswer: %v", err)
		return
	}
	if err := pc.SetLocalDescription(answer); err != nil {
		logf("[tela] erro SetLocalDescription: %v", err)
		return
	}
	postJSON("/agent/signal", map[string]any{
		"token": cfg.Token, "session_id": sessionID, "answer_sdp": answer.SDP,
	})
	logf("[tela] resposta enviada, aguardando conexão…")

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
	var frameID uint32

	for {
		select {
		case <-stop:
			return
		default:
		}
		// Intervalo conforme os quadros por segundo escolhidos (dinâmico).
		fps := atomic.LoadInt32(&streamFPS)
		if fps < 1 {
			fps = 1
		}
		time.Sleep(time.Second / time.Duration(fps))

		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}
		// Controle de congestionamento: se a fila de envio está cheia,
		// pula este quadro para não acumular atraso (mantém fluidez).
		if dc.BufferedAmount() > maxBufferedKB*1024 {
			continue
		}
		raw := captureFrame(currentMonitor())
		if raw == nil {
			if frameID == 0 {
				logf("[tela] captura retornou vazio (monitor %d)", currentMonitor())
			}
			continue
		}
		img := downscale(raw)
		var buf bytes.Buffer
		q := int(atomic.LoadInt32(&streamQuality))
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			logf("[tela] erro ao codificar JPEG: %v", err)
			continue
		}
		if frameID == 0 {
			logf("[tela] primeiro frame capturado: %dx%d, %d bytes", img.Bounds().Dx(), img.Bounds().Dy(), buf.Len())
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
	T       string  `json:"t"`      // move, down, up, scroll, key, text
	X       float64 `json:"x"`      // normalizado 0..1
	Y       float64 `json:"y"`      // normalizado 0..1
	Button  string  `json:"button"` // left, right, middle
	Delta   float64 `json:"delta"`  // scroll
	Key     string  `json:"key"`    // tecla
	Code    string  `json:"code"`   // código físico
	Down    bool    `json:"down"`   // keydown/keyup
	Ctrl    bool    `json:"ctrl"`
	Alt     bool    `json:"alt"`
	Shift   bool    `json:"shift"`
	Text    string  `json:"text"`
	Monitor int     `json:"monitor"` // troca de monitor (t="monitor")
	Quality int     `json:"quality"` // qualidade JPEG (t="quality")
	Width   int     `json:"width"`   // largura máxima (t="quality")
	Fps     int     `json:"fps"`     // quadros por segundo (t="quality")
}

func handleInput(data []byte) {
	var ev inputEvent
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.T {
	case "quality":
		applyQuality(ev)
	case "monitor":
		atomic.StoreInt32(&streamMonitor, int32(ev.Monitor))
	default:
		injectInput(ev)
	}
}

// image.Image auxiliar para evitar import não usado em builds sem captura.
var _ = image.RGBA{}
