package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"gopkg.in/yaml.v3"
)

type BatteryState string

type BatteryEvent struct {
	Percentage int
	State      BatteryState
	Timestamp  time.Time
}

type ActionConfig struct {
	TriggerOnState      string   `yaml:"trigger_on_state"`
	ThresholdPercentage int      `yaml:"threshold_percentage"`
	Operator            string   `yaml:"operator"`
	Command             []string `yaml:"command"`
}

type Config struct {
	Actions []ActionConfig `yaml:"actions"`
}

type EventBus struct {
	mu          sync.RWMutex
	subscribers []chan BatteryEvent
}

func NewEventBus() *EventBus {
	return &EventBus{subscribers: make([]chan BatteryEvent, 0)}
}

func (eb *EventBus) Subscribe() <-chan BatteryEvent {
	eb.mu.Lock()
	defer eb.mu.Unlock()
	ch := make(chan BatteryEvent, 10)
	eb.subscribers = append(eb.subscribers, ch)
	return ch
}

func (eb *EventBus) Publish(event BatteryEvent) {
	eb.mu.RLock()
	defer eb.mu.RUnlock()
	for _, ch := range eb.subscribers {
		select {
		case ch <- event:
		default:
		}
	}
}

type Monitor struct {
	bus *EventBus
}

func NewMonitor(bus *EventBus) *Monitor {
	return &Monitor{bus: bus}
}

func (m *Monitor) Start(ctx context.Context) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("failed to connect to system bus: %w", err)
	}
	defer conn.Close()

	var displayDevice dbus.ObjectPath
	obj := conn.Object("org.freedesktop.UPower", "/org/freedesktop/UPower")
	err = obj.Call("org.freedesktop.UPower.GetDisplayDevice", 0).Store(&displayDevice)
	if err != nil {
		return fmt.Errorf("failed to get battery device: %w", err)
	}

	rule := fmt.Sprintf(
		"type='signal',sender='org.freedesktop.UPower',path='%s',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged'",
		displayDevice,
	)
	call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule)
	if call.Err != nil {
		return fmt.Errorf("failed to register dbus match rule: %w", call.Err)
	}

	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)

	m.fetchAndPublish(conn, displayDevice)
	lastState := m.readState(conn, displayDevice)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c:
			currentState := m.readState(conn, displayDevice)
			if currentState != lastState {
				lastState = currentState
				time.Sleep(1 * time.Second)
			}
			m.fetchAndPublish(conn, displayDevice)
		}
	}
}

func (m *Monitor) readState(conn *dbus.Conn, device dbus.ObjectPath) BatteryState {
	batteryObj := conn.Object("org.freedesktop.UPower", device)
	stateVar, err := batteryObj.GetProperty("org.freedesktop.UPower.Device.State")
	if err != nil {
		return ""
	}
	switch stateVar.Value().(uint32) {
	case 1:
		return "Charging"
	case 2:
		return "Discharging"
	case 4:
		return "Full"
	default:
		return "Unknown"
	}
}

func (m *Monitor) fetchAndPublish(conn *dbus.Conn, device dbus.ObjectPath) {
	batteryObj := conn.Object("org.freedesktop.UPower", device)

	percentageVar, err := batteryObj.GetProperty("org.freedesktop.UPower.Device.Percentage")
	if err != nil {
		return
	}
	stateVar, err := batteryObj.GetProperty("org.freedesktop.UPower.Device.State")
	if err != nil {
		return
	}

	percent := int(percentageVar.Value().(float64))
	stateEnum := stateVar.Value().(uint32)

	var state BatteryState
	switch stateEnum {
	case 1:
		state = "Charging"
	case 2:
		state = "Discharging"
	case 4:
		state = "Full"
	default:
		state = "Unknown"
	}

	m.bus.Publish(BatteryEvent{
		Percentage: percent,
		State:      state,
		Timestamp:  time.Now(),
	})
}

func executeCommand(action ActionConfig, event BatteryEvent) {
	if len(action.Command) == 0 {
		return
	}

	processedCmd := make([]string, len(action.Command))
	for i, arg := range action.Command {
		arg = strings.ReplaceAll(arg, "{percent}", strconv.Itoa(event.Percentage))
		arg = strings.ReplaceAll(arg, "{state}", string(event.State))
		processedCmd[i] = arg
	}

	go func() {
		cmd := exec.Command(processedCmd[0], processedCmd[1:]...)
		if err := cmd.Run(); err != nil {
			fmt.Printf("command failed: %v (cmd: %s)\n", err, processedCmd[0])
		}
	}()
}

func loadConfig() (Config, string, error) {
	var cfg Config
	var pathsToTry []string

	if cwd, err := os.Getwd(); err == nil {
		pathsToTry = append(pathsToTry, filepath.Join(cwd, "config.yaml"))
	}

	if homeDir, err := os.UserHomeDir(); err == nil {
		pathsToTry = append(pathsToTry, filepath.Join(homeDir, ".config", "tangle", "config.yaml"))
	}

	pathsToTry = append(pathsToTry, "/etc/tangle/config.yaml")
	pathsToTry = append(pathsToTry, "/var/lib/tangle/config.yaml")

	var finalPath string
	var fileBytes []byte
	var err error

	for _, path := range pathsToTry {
		fileBytes, err = os.ReadFile(path)
		if err == nil {
			finalPath = path
			break
		}
	}

	if finalPath == "" {
		return cfg, "", fmt.Errorf("could not find config.yaml")
	}

	err = yaml.Unmarshal(fileBytes, &cfg)
	if err != nil {
		return cfg, finalPath, fmt.Errorf("failed to parse yaml from %s: %w", finalPath, err)
	}

	return cfg, finalPath, nil
}

func main() {
	cfg, _, err := loadConfig()
	if err != nil {
		panic(err)
	}

	bus := NewEventBus()
	monitor := NewMonitor(bus)

	actionTriggered := make(map[int]bool)
	lastTriggered := make(map[int]time.Time)
	const cooldown = 30 * time.Second

	go func() {
		events := bus.Subscribe()
		for event := range events {
			for idx, action := range cfg.Actions {
				if string(event.State) != action.TriggerOnState {
					actionTriggered[idx] = false
					continue
				}

				match := false
				switch action.Operator {
				case "any":
					match = true
				case "==":
					match = event.Percentage == action.ThresholdPercentage
				case "<=":
					match = event.Percentage <= action.ThresholdPercentage
				case ">=":
					match = event.Percentage >= action.ThresholdPercentage
				}

				if match {
					if !actionTriggered[idx] || time.Since(lastTriggered[idx]) > cooldown {
						executeCommand(action, event)
						actionTriggered[idx] = true
						lastTriggered[idx] = time.Now()
					}
				} else {
					actionTriggered[idx] = false
				}
			}
		}
	}()

	if err := monitor.Start(context.Background()); err != nil {
		panic(err)
	}
}
