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

// batteryStateFromEnum maps the UPower Device.State enum to our BatteryState type.
func batteryStateFromEnum(v uint32) BatteryState {
	switch v {
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

// fetch performs a single, consistent read of the battery's percentage and
// state. Both readState and fetchAndPublish used to exist separately and
// issue their own independent GetProperty calls, which meant a state
// transition could be observed differently by each call (a narrow race) and
// wasted an extra D-Bus round trip. This is now the single source of truth.
func (m *Monitor) fetch(conn *dbus.Conn, device dbus.ObjectPath) (BatteryEvent, error) {
	batteryObj := conn.Object("org.freedesktop.UPower", device)

	percentageVar, err := batteryObj.GetProperty("org.freedesktop.UPower.Device.Percentage")
	if err != nil {
		return BatteryEvent{}, fmt.Errorf("failed to read percentage: %w", err)
	}
	stateVar, err := batteryObj.GetProperty("org.freedesktop.UPower.Device.State")
	if err != nil {
		return BatteryEvent{}, fmt.Errorf("failed to read state: %w", err)
	}

	// Type-assert defensively. A bad assertion here used to panic and take
	// down the whole daemon if UPower ever returned an unexpected variant
	// type (e.g. during device hot-plug/teardown, or on a nonstandard
	// implementation).
	percentF, ok := percentageVar.Value().(float64)
	if !ok {
		return BatteryEvent{}, fmt.Errorf("unexpected type for Percentage: %T", percentageVar.Value())
	}
	stateEnum, ok := stateVar.Value().(uint32)
	if !ok {
		return BatteryEvent{}, fmt.Errorf("unexpected type for State: %T", stateVar.Value())
	}

	return BatteryEvent{
		Percentage: int(percentF),
		State:      batteryStateFromEnum(stateEnum),
		Timestamp:  time.Now(),
	}, nil
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

	// Register the channel with the connection BEFORE adding the match
	// rule. Previously AddMatch ran first, which opened a narrow window
	// where the bus could start routing matching signals to this
	// connection before anything was listening on c, silently dropping
	// them.
	c := make(chan *dbus.Signal, 10)
	conn.Signal(c)
	defer conn.RemoveSignal(c)

	rule := fmt.Sprintf(
		"type='signal',sender='org.freedesktop.UPower',path='%s',interface='org.freedesktop.DBus.Properties',member='PropertiesChanged'",
		displayDevice,
	)
	call := conn.BusObject().Call("org.freedesktop.DBus.AddMatch", 0, rule)
	if call.Err != nil {
		return fmt.Errorf("failed to register dbus match rule: %w", call.Err)
	}
	defer conn.BusObject().Call("org.freedesktop.DBus.RemoveMatch", 0, rule)

	initial, err := m.fetch(conn, displayDevice)
	if err != nil {
		return fmt.Errorf("failed initial battery read: %w", err)
	}
	m.bus.Publish(initial)
	lastState := initial.State

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c:
			reading, err := m.fetch(conn, displayDevice)
			if err != nil {
				// Don't crash on a transient read failure; just skip
				// this event and wait for the next signal.
				continue
			}

			if reading.State != lastState {
				lastState = reading.State
				// Give UPower a moment for the percentage to settle
				// after a state transition, but stay responsive to
				// shutdown instead of blocking unconditionally.
				select {
				case <-time.After(1 * time.Second):
				case <-ctx.Done():
					return nil
				}
				reading, err = m.fetch(conn, displayDevice)
				if err != nil {
					continue
				}
			}

			m.bus.Publish(reading)
		}
	}
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

	// actionTriggered and lastMatchedAt are only ever read/written from
	// within the single event-processing goroutine below, so no
	// additional locking is required for them.
	//
	// actionTriggered tracks whether an action has already fired for the
	// current "streak" of matching events, so it fires once per streak
	// instead of repeatedly for as long as the condition holds true.
	// lastMatchedAt tracks the last time the action's condition matched,
	// so a brief flicker below the cooldown window (e.g. a percentage
	// reading bouncing across a threshold) doesn't count as the streak
	// ending and re-arm the action.
	actionTriggered := make(map[int]bool)
	lastMatchedAt := make(map[int]time.Time)
	const cooldown = 30 * time.Second

	go func() {
		events := bus.Subscribe()
		for event := range events {
			for idx, action := range cfg.Actions {
				if string(event.State) != action.TriggerOnState {
					if actionTriggered[idx] && time.Since(lastMatchedAt[idx]) > cooldown {
						actionTriggered[idx] = false
					}
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
					if !actionTriggered[idx] {
						executeCommand(action, event)
						actionTriggered[idx] = true
					}
					lastMatchedAt[idx] = time.Now()
				} else if actionTriggered[idx] && time.Since(lastMatchedAt[idx]) > cooldown {
					actionTriggered[idx] = false
				}
			}
		}
	}()

	if err := monitor.Start(context.Background()); err != nil {
		panic(err)
	}
}
