package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

var debug bool

func init() {
	flag.BoolVar(&debug, "debug", false, "enable debug logging")
	flag.Parse()
}

var logger = log.New(os.Stdout, "[tangle] ", log.LstdFlags|log.Lshortfile)

func logf(format string, v ...any) {
	if debug {
		logger.Printf(format, v...)
	}
}

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
	bus           *EventBus
	batteryDir    string
	acDir         string
	pollInterval  time.Duration
	stateDebounce int
	lastStates    []BatteryState
}

func NewMonitor(bus *EventBus) *Monitor {
	return &Monitor{
		bus:           bus,
		pollInterval:  1 * time.Second,
		stateDebounce: 5, // reads before accepting a state change
		lastStates:    make([]BatteryState, 0, 3),
	}
}

func (m *Monitor) findBatteryAndAC() error {
	logf("scanning /sys/class/power_supply for battery and AC devices")
	entries, err := os.ReadDir("/sys/class/power_supply")
	if err != nil {
		return fmt.Errorf("failed to read /sys/class/power_supply: %w", err)
	}

	for _, entry := range entries {
		typePath := filepath.Join("/sys/class/power_supply", entry.Name(), "type")
		typeBytes, err := os.ReadFile(typePath)
		if err != nil {
			continue
		}
		devType := strings.TrimSpace(string(typeBytes))
		if devType == "Battery" && m.batteryDir == "" {
			m.batteryDir = filepath.Join("/sys/class/power_supply", entry.Name())
			logf("found battery: %s", m.batteryDir)
		} else if devType == "Mains" && m.acDir == "" {
			m.acDir = filepath.Join("/sys/class/power_supply", entry.Name())
			logf("found AC adapter: %s", m.acDir)
		}
	}

	if m.batteryDir == "" {
		return fmt.Errorf("no battery found in /sys/class/power_supply")
	}
	logf("using battery: %s, AC: %s", m.batteryDir, m.acDir)
	return nil
}

func (m *Monitor) readBattery() (BatteryEvent, error) {
	capacityBytes, err := os.ReadFile(filepath.Join(m.batteryDir, "capacity"))
	if err != nil {
		return BatteryEvent{}, fmt.Errorf("failed to read capacity: %w", err)
	}
	percentage, err := strconv.Atoi(strings.TrimSpace(string(capacityBytes)))
	if err != nil {
		return BatteryEvent{}, fmt.Errorf("failed to parse capacity: %w", err)
	}

	statusBytes, err := os.ReadFile(filepath.Join(m.batteryDir, "status"))
	if err != nil {
		return BatteryEvent{}, fmt.Errorf("failed to read status: %w", err)
	}
	status := strings.TrimSpace(string(statusBytes))

	state := BatteryState(status)
	if state == "Charging" || state == "Discharging" || state == "Full" || state == "Not charging" {
	} else {
		state = "Unknown"
	}

	logf("battery: %d%% %s", percentage, state)

	return BatteryEvent{
		Percentage: percentage,
		State:      state,
		Timestamp:  time.Now(),
	}, nil
}

func (m *Monitor) debouncedRead() (BatteryEvent, BatteryState, bool) {
	reading, err := m.readBattery()
	if err != nil {
		return BatteryEvent{}, "", false
	}

	// Add to rolling window
	m.lastStates = append(m.lastStates, reading.State)
	if len(m.lastStates) > m.stateDebounce {
		m.lastStates = m.lastStates[1:]
	}

	// Check if we have enough consistent readings
	if len(m.lastStates) < m.stateDebounce {
		return reading, reading.State, false // not enough samples yet
	}

	// Check if all recent states are the same
	consistent := true
	for _, s := range m.lastStates {
		if s != m.lastStates[0] {
			consistent = false
			break
		}
	}

	debouncedState := reading.State
	if consistent {
		debouncedState = m.lastStates[0]
	}

	return reading, debouncedState, consistent
}

func (m *Monitor) Start(ctx context.Context) error {
	logf("starting battery monitor (poll interval: %v, debounce: %d)", m.pollInterval, m.stateDebounce)
	if err := m.findBatteryAndAC(); err != nil {
		return err
	}

	var initial BatteryEvent
	for i := 0; i < m.stateDebounce; i++ {
		var err error
		initial, err = m.readBattery()
		if err != nil {
			return fmt.Errorf("failed initial battery read: %w", err)
		}
	}
	m.bus.Publish(initial)
	lastState := initial.State
	logf("initial state: %d%% %s", initial.Percentage, initial.State)

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logf("context cancelled, stopping monitor")
			return nil
		case <-ticker.C:
			reading, debouncedState, consistent := m.debouncedRead()
			if !consistent {
				continue
			}

			if debouncedState != lastState {
				logf("state changed: %s -> %s", lastState, debouncedState)
				lastState = debouncedState
				select {
				case <-time.After(250 * time.Millisecond):
				case <-ctx.Done():
					return nil
				}
				// Re-read after debounce to get fresh percentage
				reading, err := m.readBattery()
				if err != nil {
					continue
				}
				// Use debounced state but fresh percentage
				reading.State = debouncedState
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

	logf("executing command: %v", processedCmd)
	go func() {
		cmd := exec.Command(processedCmd[0], processedCmd[1:]...)
		if err := cmd.Run(); err != nil {
			logf("command failed: %v (cmd: %s)", err, processedCmd[0])
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
			logf("loaded config from: %s", path)
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

	logf("loaded %d action(s)", len(cfg.Actions))
	return cfg, finalPath, nil
}

func main() {
	logf("starting tangle battery monitor")
	cfg, _, err := loadConfig()
	if err != nil {
		logger.Fatalf("config error: %v", err)
	}

	bus := NewEventBus()
	monitor := NewMonitor(bus)

	actionTriggered := make(map[int]bool)
	lastMatchedAt := make(map[int]time.Time)
	const cooldown = 30 * time.Second

	go func() {
		events := bus.Subscribe()
		for event := range events {
			logf("received event: %d%% %s", event.Percentage, event.State)
			for idx, action := range cfg.Actions {
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

				stateMatches := string(event.State) == action.TriggerOnState
				if !stateMatches {
					actionTriggered[idx] = false
					continue
				}

				if match {
					if !actionTriggered[idx] {
						logf("action %d triggered: %v", idx, action.Command)
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
		logger.Fatalf("monitor error: %v", err)
	}
}
