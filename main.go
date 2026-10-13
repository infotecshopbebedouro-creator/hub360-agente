// =====================================================================
//  Hub360 — Agente de Suporte (Fase 1: vínculo, status, informações, comandos)
//  Linguagem: Go (gratuito). Gera um .exe único para Windows.
//
//  O agente conversa SÓ com o canal da Cloudflare (Worker), nunca direto
//  com o Base44. Fluxo:
//    1. Na primeira vez, pede o CÓDIGO DE VÍNCULO (do link do cliente) e
//       registra o computador. Guarda o token em
//       C:\ProgramData\Hub360Agente\config.json
//    2. Em loop, a cada POLL_SECONDS, manda "estou online" e pega comandos.
//    3. Executa os comandos e devolve o resultado.
//
//  Fase 1 é um programa de console (para testar). A Fase 2 vira serviço.
//
//  Compilar (na máquina Windows, com Go instalado):
//    set GOOS=windows
//    set GOARCH=amd64
//    go build -ldflags="-s -w" -o AgenteSuporte.exe main.go
// =====================================================================

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ---- Configuração fixa de compilação -------------------------------
// URL do canal (Worker da Cloudflare). Trocar aqui se mudar o Worker.
const BridgeURL = "https://hub360-agentetoqueemimplantar.infotecshopbebedouro.workers.dev"

const AgentVersion = "1.3.0"
const PollSeconds = 15

// ---- Armazenamento local -------------------------------------------

type Config struct {
	Token    string `json:"token"`
	DeviceID string `json:"device_id"`
	Group    string `json:"group_name"`
	Company  string `json:"company_name"`
}

func configDir() string {
	base := os.Getenv("ProgramData")
	if base == "" {
		base, _ = os.UserConfigDir()
	}
	dir := filepath.Join(base, "Hub360Agente")
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func configPath() string { return filepath.Join(configDir(), "config.json") }

// logf grava no arquivo de log (não há console no modo janela).
func logf(format string, a ...any) {
	line := time.Now().Format("2006-01-02 15:04:05") + " " + fmt.Sprintf(format, a...) + "\r\n"
	f, err := os.OpenFile(filepath.Join(configDir(), "agente.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		f.WriteString(line)
		f.Close()
	}
}

// psVisible roda PowerShell SEM esconder a janela — necessário para que
// caixas de diálogo (InputBox/MessageBox) apareçam de fato na tela.
func psVisible(script string) string {
	c := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-STA", "-Command", script)
	out, _ := c.Output()
	return strings.TrimSpace(string(out))
}

// askCodeDialog mostra uma caixa para digitar o código (sem console).
func askCodeDialog() string {
	logf("Pedindo código de vínculo na tela…")
	out := psVisible(`Add-Type -AssemblyName Microsoft.VisualBasic | Out-Null; [Microsoft.VisualBasic.Interaction]::InputBox("Digite o codigo de vinculo do suporte (ex.: K7QM-4XPA):","Hub360 - Agente de Suporte","")`)
	return strings.TrimSpace(out)
}

// notify mostra um aviso rápido na tela.
func notify(title, msg string) {
	safe := strings.ReplaceAll(msg, "\"", "'")
	go psVisible(`Add-Type -AssemblyName System.Windows.Forms | Out-Null; [System.Windows.Forms.MessageBox]::Show("` + safe + `","` + strings.ReplaceAll(title, "\"", "'") + `")`)
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func saveConfig(c *Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}

// ---- Comunicação com o canal ---------------------------------------

func postJSON(path string, payload map[string]any) (map[string]any, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", BridgeURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out, nil
}

// ---- Coleta de informações do micro (Windows) ----------------------

func run(cmd string, args ...string) string {
	c := exec.Command(cmd, args...)
	c.SysProcAttr = hideWindow() // definido no arquivo específico de SO
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	_ = c.Run()
	return strings.TrimSpace(out.String())
}

func ps(script string) string {
	return run("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
}

func collectSysInfo() map[string]any {
	info := map[string]any{}
	if runtime.GOOS != "windows" {
		info["os_version"] = runtime.GOOS
		return info
	}

	info["os_version"] = ps(`(Get-CimInstance Win32_OperatingSystem).Caption`)
	info["os_build"] = ps(`(Get-CimInstance Win32_OperatingSystem).Version`)
	info["cpu"] = ps(`(Get-CimInstance Win32_Processor).Name`)
	info["motherboard"] = ps(`(Get-CimInstance Win32_BaseBoard).Product`)
	info["serial"] = ps(`(Get-CimInstance Win32_BIOS).SerialNumber`)
	info["antivirus"] = ps(`(Get-CimInstance -Namespace root\SecurityCenter2 -Class AntiVirusProduct -ErrorAction SilentlyContinue | Select-Object -First 1).displayName`)

	// Memória (GB)
	if totKB := ps(`(Get-CimInstance Win32_OperatingSystem).TotalVisibleMemorySize`); totKB != "" {
		if n, err := strconv.ParseFloat(totKB, 64); err == nil {
			info["ram_total"] = fmt.Sprintf("%.1f GB", n/1024/1024)
		}
	}
	if freeKB := ps(`(Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory`); freeKB != "" {
		if n, err := strconv.ParseFloat(freeKB, 64); err == nil {
			info["ram_free"] = fmt.Sprintf("%.1f GB", n/1024/1024)
		}
	}

	// Tempo ligado
	if up := ps(`$u=(Get-Date)-(Get-CimInstance Win32_OperatingSystem).LastBootUpTime; "{0}d {1}h {2}m" -f $u.Days,$u.Hours,$u.Minutes`); up != "" {
		info["uptime"] = up
	}

	// Ativação do Windows
	act := ps(`(Get-CimInstance SoftwareLicensingProduct -Filter "ApplicationID='55c92734-d682-4d71-983e-d6ec3f16059f' AND LicenseStatus=1" -ErrorAction SilentlyContinue | Select-Object -First 1) -ne $null`)
	info["os_activated"] = strings.EqualFold(act, "True")

	// Discos
	disksJSON := ps(`Get-CimInstance Win32_LogicalDisk -Filter "DriveType=3" | ForEach-Object { [pscustomobject]@{ drive=$_.DeviceID; total=[math]::Round($_.Size/1GB,0); free=[math]::Round($_.FreeSpace/1GB,0) } } | ConvertTo-Json -Compress`)
	info["disks"] = parseDisks(disksJSON)

	// Rede
	netJSON := ps(`Get-NetIPConfiguration | Where-Object { $_.IPv4Address } | ForEach-Object { [pscustomobject]@{ iface=$_.InterfaceAlias; ip=$_.IPv4Address.IPAddress; mac=(Get-NetAdapter -InterfaceIndex $_.InterfaceIndex -ErrorAction SilentlyContinue).MacAddress } } | ConvertTo-Json -Compress`)
	info["network"] = parseNetwork(netJSON)

	// Impressoras
	printers := ps(`(Get-Printer -ErrorAction SilentlyContinue | Select-Object -ExpandProperty Name) -join "|"`)
	if printers != "" {
		info["printers"] = strings.Split(printers, "|")
	}

	// Nº de programas instalados
	cnt := ps(`(Get-ItemProperty HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\*,HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\* -ErrorAction SilentlyContinue | Where-Object { $_.DisplayName }).Count`)
	if n, err := strconv.Atoi(cnt); err == nil {
		info["installed_apps_count"] = n
	}

	// Atualizações pendentes (best-effort)
	upd := ps(`try { (New-Object -ComObject Microsoft.Update.Session).CreateUpdateSearcher().Search("IsInstalled=0").Updates.Count } catch { "" }`)
	if n, err := strconv.Atoi(upd); err == nil {
		info["updates_pending"] = n
	}

	info["alerts"] = buildAlerts(info)
	return info
}

func parseDisks(j string) []map[string]any {
	var out []map[string]any
	j = strings.TrimSpace(j)
	if j == "" {
		return out
	}
	if strings.HasPrefix(j, "{") {
		j = "[" + j + "]"
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(j), &raw); err != nil {
		return out
	}
	for _, d := range raw {
		out = append(out, map[string]any{
			"drive": fmt.Sprintf("%v", d["drive"]),
			"total": fmt.Sprintf("%v GB", d["total"]),
			"free":  fmt.Sprintf("%v GB", d["free"]),
		})
	}
	return out
}

func parseNetwork(j string) []map[string]any {
	var out []map[string]any
	j = strings.TrimSpace(j)
	if j == "" {
		return out
	}
	if strings.HasPrefix(j, "{") {
		j = "[" + j + "]"
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(j), &raw); err != nil {
		return out
	}
	for _, n := range raw {
		out = append(out, map[string]any{
			"iface": fmt.Sprintf("%v", n["iface"]),
			"ip":    fmt.Sprintf("%v", n["ip"]),
			"mac":   fmt.Sprintf("%v", n["mac"]),
		})
	}
	return out
}

func buildAlerts(info map[string]any) []string {
	var alerts []string
	// Disco cheio (<10% livre)
	if disks, ok := info["disks"].([]map[string]any); ok {
		for _, d := range disks {
			tot := parseGB(fmt.Sprintf("%v", d["total"]))
			free := parseGB(fmt.Sprintf("%v", d["free"]))
			if tot > 0 && free/tot < 0.1 {
				alerts = append(alerts, fmt.Sprintf("Disco %v com pouco espaço (%v livre de %v)", d["drive"], d["free"], d["total"]))
			}
		}
	}
	if act, ok := info["os_activated"].(bool); ok && !act {
		alerts = append(alerts, "Windows não ativado")
	}
	if n, ok := info["updates_pending"].(int); ok && n > 20 {
		alerts = append(alerts, fmt.Sprintf("%d atualizações pendentes", n))
	}
	return alerts
}

func parseGB(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(s, " GB"))
	n, _ := strconv.ParseFloat(s, 64)
	return n
}

// ---- Execução de comandos ------------------------------------------

func execCommand(cmd map[string]any) (status, result string, sysinfo map[string]any) {
	kind, _ := cmd["kind"].(string)
	preset, _ := cmd["preset_key"].(string)
	payload, _ := cmd["payload"].(string)

	switch kind {
	case "collect_info":
		return "done", "Informações coletadas.", collectSysInfo()
	case "message":
		// Aviso na tela do usuário. Usa Wscript.Shell Popup, que funciona em
		// qualquer edição do Windows (inclusive Home, onde msg.exe não existe).
		// Não bloqueia o agente (roda em segundo plano, fecha sozinho em 60s).
		go func(text string) {
			safe := strings.ReplaceAll(text, "\"", "'")
			ps(`(New-Object -ComObject Wscript.Shell).Popup("` + safe + `",60,"Suporte Técnico",64)`)
		}(payload)
		return "done", "Mensagem exibida.", nil
	case "lock":
		run("rundll32.exe", "user32.dll,LockWorkStation")
		return "done", "Tela bloqueada.", nil
	case "restart":
		go func() {
			time.Sleep(2 * time.Second)
			run("shutdown", "/r", "/t", "5", "/c", "Reinício solicitado pelo suporte")
		}()
		return "done", "Reinício agendado (5s).", nil
	case "shutdown":
		go func() {
			time.Sleep(2 * time.Second)
			run("shutdown", "/s", "/t", "5", "/c", "Desligamento solicitado pelo suporte")
		}()
		return "done", "Desligamento agendado (5s).", nil
	case "logoff":
		go func() { time.Sleep(2 * time.Second); run("shutdown", "/l") }()
		return "done", "Encerrando sessão.", nil
	case "preset":
		return "done", runPreset(preset), nil
	case "script":
		out := ps(payload)
		if out == "" {
			out = "(sem saída)"
		}
		return "done", out, nil
	case "routine":
		return "done", runRoutine(payload), nil
	default:
		return "error", "Comando desconhecido: " + kind, nil
	}
}

func runPreset(key string) string {
	switch key {
	case "clear_temp":
		ps(`Remove-Item "$env:TEMP\*" -Recurse -Force -ErrorAction SilentlyContinue; Remove-Item "C:\Windows\Temp\*" -Recurse -Force -ErrorAction SilentlyContinue`)
		return "Temporários limpos."
	case "flush_dns":
		return run("ipconfig", "/flushdns")
	case "restart_spooler":
		ps(`Restart-Service -Name Spooler -Force`)
		return "Spooler reiniciado."
	case "winget_upgrade":
		out := run("winget", "upgrade", "--all", "--silent", "--accept-source-agreements", "--accept-package-agreements")
		if out == "" {
			out = "winget executado."
		}
		return out
	case "sfc_scan":
		return run("sfc", "/scannow")
	case "gpupdate":
		return run("gpupdate", "/force")
	default:
		return "Comando pronto desconhecido: " + key
	}
}

func runRoutine(payload string) string {
	var steps []map[string]any
	if err := json.Unmarshal([]byte(payload), &steps); err != nil {
		return "Rotina inválida."
	}
	var log []string
	for i, s := range steps {
		st, res, _ := execCommand(s)
		log = append(log, fmt.Sprintf("Passo %d (%v): %s — %s", i+1, s["kind"], st, trunc(res, 200)))
	}
	return strings.Join(log, "\n")
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// ---- Registro e loop principal -------------------------------------

// Obtém o código de vínculo: argumento de linha de comando, arquivo codigo.txt
// ao lado do .exe, ou uma caixa de diálogo na tela (funciona sem console).
func getBindCode() string {
	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) != "" {
		return strings.TrimSpace(os.Args[1])
	}
	if exe, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "codigo.txt")); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
	}
	return askCodeDialog()
}

func registerAgent(code string) (*Config, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, fmt.Errorf("código não informado")
	}
	host, _ := os.Hostname()
	uid := machineUID()
	user := os.Getenv("USERNAME")

	resp, err := postJSON("/agent/register", map[string]any{
		"code":        code,
		"machine_uid": uid,
		"hostname":    host,
		"os":          ps(`(Get-CimInstance Win32_OperatingSystem).Caption`),
		"user":        user,
		"version":     AgentVersion,
		"monitors":    monitorCount(),
	})
	if err != nil {
		return nil, err
	}
	if errMsg, ok := resp["error"].(string); ok && errMsg != "" {
		return nil, fmt.Errorf("%s", errMsg)
	}
	tok, _ := resp["token"].(string)
	if tok == "" {
		return nil, fmt.Errorf("resposta sem token")
	}
	c := &Config{
		Token:    tok,
		DeviceID: fmt.Sprintf("%v", resp["device_id"]),
		Group:    fmt.Sprintf("%v", resp["group_name"]),
		Company:  fmt.Sprintf("%v", resp["company_name"]),
	}
	_ = saveConfig(c)
	logf("Vinculado com sucesso ao grupo: %s", c.Group)
	return c, nil
}

func pollLoop(c *Config) {
	logf("Agente em execução (grupo %s).", c.Group)
	for {
		host, _ := os.Hostname()
		resp, err := postJSON("/agent/poll", map[string]any{
			"token":    c.Token,
			"version":  AgentVersion,
			"user":     os.Getenv("USERNAME"),
			"monitors": monitorCount(),
		})
		_ = host
		if err == nil && resp != nil {
			// Sessão de acesso remoto pendente → inicia a transmissão da tela
			if ps, ok := resp["pending_session"].(map[string]any); ok && ps != nil {
				if sid, ok := ps["id"].(string); ok && sid != "" {
					logf("Sessão de tela pendente: %s", sid)
					go startRemoteSession(c, sid)
				}
			}
			if cmds, ok := resp["commands"].([]any); ok {
				for _, raw := range cmds {
					if cmd, ok := raw.(map[string]any); ok {
						id, _ := cmd["id"].(string)
						status, result, sysinfo := execCommand(cmd)
						report := map[string]any{"token": c.Token, "command_id": id, "status": status, "result_text": result}
						if sysinfo != nil {
							report["sysinfo"] = sysinfo
						}
						_, _ = postJSON("/agent/report", report)
					}
				}
			}
		}
		time.Sleep(PollSeconds * time.Second)
	}
}

// openTicketDialog: o cliente abre um chamado pela bandeja.
func openTicketDialog(c *Config) {
	title := psVisible(`Add-Type -AssemblyName Microsoft.VisualBasic | Out-Null; [Microsoft.VisualBasic.Interaction]::InputBox("Resuma o problema (ex.: Impressora nao imprime):","Abrir chamado de suporte","")`)
	title = strings.TrimSpace(title)
	if title == "" {
		return
	}
	desc := psVisible(`Add-Type -AssemblyName Microsoft.VisualBasic | Out-Null; [Microsoft.VisualBasic.Interaction]::InputBox("Descreva com mais detalhes (opcional):","Abrir chamado de suporte","")`)
	name := os.Getenv("USERNAME")
	resp, err := postJSON("/agent/ticket", map[string]any{
		"token":         c.Token,
		"customer_name": name,
		"title":         title,
		"description":   strings.TrimSpace(desc) + "\n\n(Aberto pelo Agente de Suporte)",
	})
	if err != nil || resp == nil {
		notify("Suporte", "Não foi possível abrir o chamado agora. Tente novamente.")
		return
	}
	if num, ok := resp["ticket_number"].(string); ok && num != "" {
		notify("Chamado aberto", "Seu chamado "+num+" foi registrado. Em breve o suporte entrará em contato.")
	} else if em, ok := resp["error"].(string); ok {
		notify("Suporte", "Erro: "+em)
	} else {
		notify("Chamado aberto", "Seu chamado foi registrado.")
	}
}

// showInfoDialog: mostra informações do PC ao cliente.
func showInfoDialog() {
	info := collectSysInfo()
	get := func(k string) string {
		if v, ok := info[k]; ok {
			return fmt.Sprintf("%v", v)
		}
		return "-"
	}
	msg := "Windows: " + get("os_version") + "\\n" +
		"Processador: " + get("cpu") + "\\n" +
		"Memoria: " + get("ram_total") + "\\n" +
		"Ligado ha: " + get("uptime")
	psVisible(`Add-Type -AssemblyName System.Windows.Forms | Out-Null; [System.Windows.Forms.MessageBox]::Show("` + msg + `","Informacoes do meu PC")`)
}

func main() {
	c, err := loadConfig()
	if err != nil || c == nil || c.Token == "" {
		code := getBindCode()
		c, err = registerAgent(code)
		if err != nil {
			logf("Erro no vínculo: %v", err)
			notify("Hub360 — Agente", "Não foi possível vincular: "+err.Error())
			return
		}
		notify("Hub360 — Agente", "Computador vinculado ao suporte: "+c.Group)
	} else {
		logf("Agente já vinculado ao grupo: %s", c.Group)
	}

	// Garante autoinício com o Windows (copia para local fixo e cria atalho de início).
	installSelf()

	// Mantém o status/comandos rodando em segundo plano…
	go pollLoop(c)
	// …e abre o ícone na bandeja (bloqueia a thread principal).
	runTray(c)
}
