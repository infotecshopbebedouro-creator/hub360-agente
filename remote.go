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
	maxBufferedKB = 400  // se o canal tiver mais que isto na fila, pula o quadro
	tileSize      = 256  // tamanho do bloco (tile) para enviar só o que mudou
	resetMarker   = 0xF0 // novo tamanho de tela (limpa o canvas)
	tileMarker    = 0xF2 // bloco atualizado
)

// Parâmetros de qualidade ajustáveis em tempo real pelo técnico.
var (
	streamQuality int32 = 38   // qualidade JPEG (1-95)
	streamWidth   int32 = 1366 // largura máxima enviada
	streamFPS     int32 = 12   // quadros por segundo alvo
	streamMonitor int32        // monitor em transmissão
	forceFull     int32        // 1 = reenviar a tela inteira no próximo quadro
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
// Sempre devolve *image.RGBA com origem (0,0), para facilitar o fatiamento.
func downscale(src image.Image) *image.RGBA {
	mw := int(atomic.LoadInt32(&streamWidth))
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= mw {
		if rgba, ok := src.(*image.RGBA); ok && b.Min.X == 0 && b.Min.Y == 0 {
			return rgba
		}
		dst := image.NewRGBA(image.Rect(0, 0, w, h))
		xdraw.Draw(dst, dst.Bounds(), src, b.Min, xdraw.Src)
		return dst
	}
	nh := h * mw / w
	dst := image.NewRGBA(image.Rect(0, 0, mw, nh))
	xdraw.NearestNeighbor.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

// Soma de verificação rápida (FNV-1a) dos pixels de um bloco.
func tileChecksum(img *image.RGBA, r image.Rectangle) uint64 {
	var h uint64 = 1469598103934665603
	// Amostra 1 a cada 3 linhas e pula pixels (passo 12 bytes = 3 pixels)
	// para detectar mudança rápido, sem varrer todos os pixels (economiza CPU).
	for y := r.Min.Y; y < r.Max.Y; y += 3 {
		i0 := img.PixOffset(r.Min.X, y)
		i1 := img.PixOffset(r.Max.X-1, y) + 4
		for i := i0; i < i1; i += 12 {
			h ^= uint64(img.Pix[i])
			h *= 1099511628211
		}
	}
	return h
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

// Captura a tela e envia SÓ os blocos (tiles) que mudaram desde o quadro
// anterior. Isso reduz muito a banda e o uso de CPU quando a tela está
// quase parada (o caso comum em suporte).
func streamFrames(dc *webrtc.DataChannel, stop chan struct{}) {
	var sums []uint64
	var cols, rows, curW, curH int
	first := true
	idle := 0
	lastKeyframe := time.Now()

	for {
		select {
		case <-stop:
			return
		default:
		}
		// Ritmo: no alvo quando há movimento; mais devagar quando a tela
		// está parada (economiza muito CPU no micro do cliente).
		fps := atomic.LoadInt32(&streamFPS)
		if fps < 1 {
			fps = 1
		}
		sleep := time.Second / time.Duration(fps)
		if idle > 3 {
			sleep = 300 * time.Millisecond // tela parada: captura devagar
		}
		time.Sleep(sleep)

		if dc.ReadyState() != webrtc.DataChannelStateOpen {
			return
		}
		if dc.BufferedAmount() > maxBufferedKB*1024 {
			continue
		}
		raw := captureFrame(currentMonitor())
		if raw == nil {
			continue
		}
		img := downscale(raw)
		w, h := img.Bounds().Dx(), img.Bounds().Dy()

		if atomic.CompareAndSwapInt32(&forceFull, 1, 0) {
			curW, curH = 0, 0
		}

		if w != curW || h != curH || first {
			curW, curH = w, h
			cols = (w + tileSize - 1) / tileSize
			rows = (h + tileSize - 1) / tileSize
			sums = make([]uint64, cols*rows)
			for i := range sums {
				sums[i] = 0xFFFFFFFFFFFFFFFF
			}
			reset := make([]byte, 5)
			reset[0] = resetMarker
			binary.LittleEndian.PutUint16(reset[1:], uint16(w))
			binary.LittleEndian.PutUint16(reset[3:], uint16(h))
			dc.Send(reset)
			first = false
			lastKeyframe = time.Now()
			logf("[tela] tela %dx%d (%d blocos)", w, h, cols*rows)
		}

		// "Keyframe" periódico: como o tráfego é sem reenvio garantido,
		// a cada 8s reenviamos tudo para curar blocos que possam ter se perdido.
		if time.Since(lastKeyframe) > 8*time.Second {
			for i := range sums {
				sums[i] = 0xFFFFFFFFFFFFFFFF
			}
			lastKeyframe = time.Now()
		}

		q := int(atomic.LoadInt32(&streamQuality))
		changed := 0
		for ty := 0; ty < rows; ty++ {
			for tx := 0; tx < cols; tx++ {
				if dc.BufferedAmount() > maxBufferedKB*1024 {
					break
				}
				x0 := tx * tileSize
				y0 := ty * tileSize
				x1 := x0 + tileSize
				y1 := y0 + tileSize
				if x1 > w {
					x1 = w
				}
				if y1 > h {
					y1 = h
				}
				rect := image.Rect(x0, y0, x1, y1)
				sum := tileChecksum(img, rect)
				idx := ty*cols + tx
				if sum == sums[idx] {
					continue
				}
				sub := img.SubImage(rect)
				var buf bytes.Buffer
				if jpeg.Encode(&buf, sub, &jpeg.Options{Quality: q}) != nil {
					continue
				}
				header := make([]byte, 9)
				header[0] = tileMarker
				binary.LittleEndian.PutUint16(header[1:], uint16(x0))
				binary.LittleEndian.PutUint16(header[3:], uint16(y0))
				binary.LittleEndian.PutUint16(header[5:], uint16(x1-x0))
				binary.LittleEndian.PutUint16(header[7:], uint16(y1-y0))
				if dc.Send(append(header, buf.Bytes()...)) != nil {
					return
				}
				sums[idx] = sum
				changed++
			}
		}
		if changed == 0 {
			idle++
		} else {
			idle = 0
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
		atomic.StoreInt32(&forceFull, 1)
	case "refresh":
		atomic.StoreInt32(&forceFull, 1)
	default:
		injectInput(ev)
	}
}

// image.Image auxiliar para evitar import não usado em builds sem captura.
var _ = image.RGBA{}
