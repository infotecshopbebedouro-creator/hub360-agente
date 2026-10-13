//go:build windows

package main

import (
	_ "embed"
	"os"

	"github.com/getlantern/systray"
)

//go:embed icon.ico
var iconData []byte

// runTray mostra o ícone ao lado do relógio com o menu de botão direito.
// systray.Run bloqueia a thread principal (necessário para a interface).
func runTray(c *Config) {
	systray.Run(func() { onTrayReady(c) }, func() {})
}

func onTrayReady(c *Config) {
	systray.SetIcon(iconData)
	systray.SetTitle("")
	name := c.Company
	if name == "" {
		name = "Suporte Técnico"
	}
	systray.SetTooltip(name + " — Agente de Suporte")

	mTicket := systray.AddMenuItem("Abrir chamado", "Abrir um chamado de suporte")
	mInfo := systray.AddMenuItem("Informações do meu PC", "Ver dados do computador")
	mGroup := systray.AddMenuItem("Vinculado a: "+c.Group, "")
	mGroup.Disable()
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Sair", "Fechar o agente de suporte")

	go func() {
		for {
			select {
			case <-mTicket.ClickedCh:
				go openTicketDialog(c)
			case <-mInfo.ClickedCh:
				go showInfoDialog()
			case <-mQuit.ClickedCh:
				systray.Quit()
				os.Exit(0)
			}
		}
	}()
}
