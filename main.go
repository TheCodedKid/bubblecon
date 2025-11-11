package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/lipgloss"
	"github.com/gorcon/rcon"
	"gopkg.in/yaml.v3"
)

// config types

type serverConfig struct {
	Name      string `yaml:"name"`
	Address   string `yaml:"address"`
	Password  string `yaml:"password"`
	Container string `yaml:"container,omitempty"` // Docker container name or ID
}

type appConfig struct {
	Servers []serverConfig `yaml:"servers"`
}

func loadConfig(path string) ([]serverConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %w", path, err)
	}

	var cfg appConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse YAML: %w", err)
	}

	if len(cfg.Servers) == 0 {
		return nil, fmt.Errorf("no servers defined in %s", path)
	}

	return cfg.Servers, nil
}

// list item

type serverItem serverConfig

func (s serverItem) Title() string       { return s.Name }
func (s serverItem) Description() string { return s.Address }
func (s serverItem) FilterValue() string { return s.Name }

// messages

type rconResultMsg struct {
	serverName string
	cmd        string
	output     string
	err        error
}

type dockerResultMsg struct {
	serverName string
	action     string
	output     string
	err        error
}

// model

type model struct {
	list        list.Model
	input       textarea.Model
	logLines    []string
	activeName  string
	width       int
	height      int
	quitting    bool
	statusLine  string
	statusTimer time.Time
	servers     []serverConfig
}

func initialModel(servers []serverConfig) model {
	items := []list.Item{}
	for _, s := range servers {
		items = append(items, serverItem(s))
	}

	delegate := list.NewDefaultDelegate()
	l := list.New(items, delegate, 24, 10)
	l.Title = "Servers"
	l.SetShowHelp(false)
	l.DisableQuitKeybindings()
	l.SetFilteringEnabled(false)

	ta := textarea.New()
	ta.Placeholder = "Type RCON command, press Enter to send"
	ta.Prompt = "> "
	ta.Focus()
	ta.SetHeight(3)
	ta.ShowLineNumbers = false

	m := model{
		list:       l,
		input:      ta,
		logLines:   []string{"Ready."},
		activeName: "",
		servers:    servers,
	}

	if len(servers) > 0 {
		m.activeName = servers[0].Name
		m.list.Select(0)
		m.pushLog(fmt.Sprintf("Active server: %s", m.activeName))
	} else {
		m.pushLog("⚠️ No servers configured. Please check config.yaml")
	}

	return m
}

// helpers

func (m *model) activeServer() *serverConfig {
	if m.activeName == "" {
		return nil
	}
	for i := range m.servers {
		if m.servers[i].Name == m.activeName {
			return &m.servers[i]
		}
	}
	return nil
}

func (m *model) pushLog(line string) {
	const maxLines = 500
	m.logLines = append(m.logLines, line)
	if len(m.logLines) > maxLines {
		m.logLines = m.logLines[len(m.logLines)-maxLines:]
	}
}

func (m *model) setStatus(msg string) {
	m.statusLine = msg
	m.statusTimer = time.Now()
}

// commands

func sendRCONCmd(s serverConfig, cmd string) tea.Cmd {
	return func() tea.Msg {
		client, err := rcon.Dial(s.Address, s.Password)
		if err != nil {
			return rconResultMsg{
				serverName: s.Name,
				cmd:        cmd,
				err:        fmt.Errorf("failed to connect: %w", err),
			}
		}
		defer client.Close()

		resp, err := client.Execute(cmd)
		return rconResultMsg{
			serverName: s.Name,
			cmd:        cmd,
			output:     resp,
			err:        err,
		}
	}
}

func dockerAction(s serverConfig, action string) tea.Cmd {
	return func() tea.Msg {
		if s.Container == "" {
			return dockerResultMsg{
				serverName: s.Name,
				action:     action,
				err:        fmt.Errorf("no container configured"),
			}
		}

		var args []string
		switch action {
		case "start":
			args = []string{"start", s.Container}
		case "stop":
			args = []string{"stop", s.Container}
		case "restart":
			args = []string{"restart", s.Container}
		case "status":
			args = []string{"inspect", "--format", "{{.State.Status}}", s.Container}
		default:
			return dockerResultMsg{
				serverName: s.Name,
				action:     action,
				err:        fmt.Errorf("unknown action: %s", action),
			}
		}

		cmd := exec.Command("docker", args...)
		output, err := cmd.CombinedOutput()
		
		return dockerResultMsg{
			serverName: s.Name,
			action:     action,
			output:     string(output),
			err:        err,
		}
	}
}

// tea.Model

func (m model) Init() tea.Cmd { return textarea.Blink }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.list.SetSize(24, m.height-5)
		m.input.SetWidth(m.width - 26)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "tab":
			total := len(m.list.Items())
			if total > 0 {
				idx := (m.list.Index() + 1) % total
				m.list.Select(idx)
				if it, ok := m.list.SelectedItem().(serverItem); ok {
					m.activeName = it.Name
					m.pushLog(fmt.Sprintf("Active server: %s", m.activeName))
				}
			}
			return m, nil
		case "ctrl+s":
			// Docker start
			s := m.activeServer()
			if s == nil {
				m.pushLog("❌ No active server selected.")
				return m, nil
			}
			if s.Container == "" {
				m.pushLog(fmt.Sprintf("[%s] ⚠️ No container configured", s.Name))
				return m, nil
			}
			m.pushLog(fmt.Sprintf("[%s] 🐳 Starting container: %s", s.Name, s.Container))
			m.setStatus("Starting container...")
			return m, dockerAction(*s, "start")
		case "ctrl+x":
			// Docker stop
			s := m.activeServer()
			if s == nil {
				m.pushLog("❌ No active server selected.")
				return m, nil
			}
			if s.Container == "" {
				m.pushLog(fmt.Sprintf("[%s] ⚠️ No container configured", s.Name))
				return m, nil
			}
			m.pushLog(fmt.Sprintf("[%s] 🐳 Stopping container: %s", s.Name, s.Container))
			m.setStatus("Stopping container...")
			return m, dockerAction(*s, "stop")
		case "ctrl+r":
			// Docker restart
			s := m.activeServer()
			if s == nil {
				m.pushLog("❌ No active server selected.")
				return m, nil
			}
			if s.Container == "" {
				m.pushLog(fmt.Sprintf("[%s] ⚠️ No container configured", s.Name))
				return m, nil
			}
			m.pushLog(fmt.Sprintf("[%s] 🐳 Restarting container: %s", s.Name, s.Container))
			m.setStatus("Restarting container...")
			return m, dockerAction(*s, "restart")
		case "ctrl+d":
			// Docker status
			s := m.activeServer()
			if s == nil {
				m.pushLog("❌ No active server selected.")
				return m, nil
			}
			if s.Container == "" {
				m.pushLog(fmt.Sprintf("[%s] ⚠️ No container configured", s.Name))
				return m, nil
			}
			m.pushLog(fmt.Sprintf("[%s] 🐳 Checking status: %s", s.Name, s.Container))
			m.setStatus("Checking status...")
			return m, dockerAction(*s, "status")
		case "enter":
			cmdStr := m.input.Value()
			m.input.Reset()
			if cmdStr == "" {
				return m, nil
			}
			s := m.activeServer()
			if s == nil {
				m.pushLog("❌ No active server selected.")
				return m, nil
			}
			m.pushLog(fmt.Sprintf("[%s] > %s", s.Name, cmdStr))
			m.setStatus("Sending...")
			return m, sendRCONCmd(*s, cmdStr)
		}

	case rconResultMsg:
		if msg.err != nil {
			m.pushLog(fmt.Sprintf("[%s] ⚠️ ERROR: %v", msg.serverName, msg.err))
			m.setStatus("Command failed")
		} else {
			out := msg.output
			if out == "" {
				out = "(no response)"
			}
			m.pushLog(fmt.Sprintf("[%s] < %s", msg.serverName, out))
			m.setStatus("OK")
		}
		return m, nil

	case dockerResultMsg:
		if msg.err != nil {
			m.pushLog(fmt.Sprintf("[%s] 🐳 ERROR: %v", msg.serverName, msg.err))
			m.setStatus(fmt.Sprintf("Docker %s failed", msg.action))
		} else {
			out := msg.output
			if out == "" {
				out = "success"
			}
			m.pushLog(fmt.Sprintf("[%s] 🐳 %s: %s", msg.serverName, msg.action, out))
			m.setStatus(fmt.Sprintf("Docker %s OK", msg.action))
		}
		return m, nil
	}

	var cmdInput, cmdList tea.Cmd
	m.input, cmdInput = m.input.Update(msg)
	m.list, cmdList = m.list.Update(msg)
	return m, tea.Batch(cmdInput, cmdList)
}

func (m model) View() string {
	if m.quitting {
		return ""
	}

	leftWidth := 24
	rightWidth := m.width - leftWidth - 2
	if rightWidth < 40 {
		rightWidth = 40
	}

	listView := lipgloss.NewStyle().Width(leftWidth).Render(m.list.View())

	logStyle := lipgloss.NewStyle().Width(rightWidth).Height(m.height - 6)
	logContent := ""
	start := 0
	if len(m.logLines) > m.height-6 {
		start = len(m.logLines) - (m.height - 6)
	}
	for i := start; i < len(m.logLines); i++ {
		logContent += m.logLines[i] + "\n"
	}
	logView := logStyle.Render(logContent)

	status := m.statusLine
	if status == "" {
		if s := m.activeServer(); s != nil {
			status = fmt.Sprintf("Active: %s (%s)", s.Name, s.Address)
			if s.Container != "" {
				status += fmt.Sprintf(" | Container: %s", s.Container)
			}
		} else {
			status = "No active server"
		}
	}
	helpText := " [Tab] switch | [Ctrl+S] start | [Ctrl+X] stop | [Ctrl+R] restart | [Ctrl+D] status | [Ctrl+C] quit"
	statusBar := lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Render(status + "\n" + helpText)

	inputView := lipgloss.NewStyle().Width(rightWidth).Render(m.input.View())
	mainRow := lipgloss.JoinHorizontal(lipgloss.Top, listView, " ", logView)

	return lipgloss.JoinVertical(lipgloss.Left, mainRow, statusBar, inputView)
}

func main() {
	cfgPath := "config.yaml"
	servers, err := loadConfig(cfgPath)
	if err != nil {
		log.Printf("⚠️ %v\n", err)
		log.Println("Tip: Ensure config.yaml exists and defines at least one server.")
		os.Exit(1)
	}

	if len(servers) == 0 {
		log.Println("⚠️ No servers found in config.yaml. Exiting.")
		os.Exit(1)
	}

	if _, err := tea.NewProgram(initialModel(servers), tea.WithAltScreen()).Run(); err != nil {
		log.Println("Error:", err)
		os.Exit(1)
	}
}
