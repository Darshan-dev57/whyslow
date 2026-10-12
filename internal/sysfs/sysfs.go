// Package sysfs reads battery, thermal and block-device facts from /sys.
//
// All of it is optional hardware: desktops and VMs have no battery, many
// VMs have no thermal zones. Absence is reported as a nil result, never as
// an error, so the rest of whyslow keeps running.
package sysfs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// FS is a /sys mount. Tests point Root at a fixture tree.
type FS struct {
	Root string
}

// New returns an FS rooted at root.
func New(root string) FS { return FS{Root: root} }

// Default is the host's /sys.
func Default() FS { return FS{Root: "/sys"} }

func (fs FS) path(elem ...string) string {
	return filepath.Join(append([]string{fs.Root}, elem...)...)
}

func readString(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

func readInt(path string) (int64, bool) {
	s, ok := readString(path)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Battery is the combined state of every system battery.
type Battery struct {
	Percent    float64 // 0..100
	Status     string  // Charging, Discharging, Full, Not charging, Unknown
	PowerWatts float64 // current draw or charge rate; 0 when not reported
	EnergyWh   float64 // remaining energy; 0 when not reported
	EnergyFull float64 // full-charge energy; 0 when not reported
	HasPower   bool    // PowerWatts came from the hardware
	HasEnergy  bool    // EnergyWh came from the hardware
	Names      []string
}

// Discharging reports whether the system runs on battery right now.
func (b *Battery) Discharging() bool { return b != nil && b.Status == "Discharging" }

// Battery returns the system battery state, or nil when there is none.
//
// Laptops report either energy_* (µWh) and power_now (µW), or charge_* (µAh)
// with current_now (µA) and voltage_now (µV); both are handled. Supplies with
// scope=Device are peripherals (mice, headsets) and are skipped.
func (fs FS) Battery() *Battery {
	dir := fs.path("class", "power_supply")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out Battery
	var full, now float64 // for percent across several batteries
	var capSum float64
	n := 0
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if t, _ := readString(filepath.Join(p, "type")); t != "Battery" {
			continue
		}
		if s, _ := readString(filepath.Join(p, "scope")); s == "Device" {
			continue
		}
		if present, ok := readInt(filepath.Join(p, "present")); ok && present == 0 {
			continue
		}
		n++
		out.Names = append(out.Names, e.Name())
		status, _ := readString(filepath.Join(p, "status"))
		if out.Status == "" || status == "Discharging" {
			out.Status = status
		}
		volt, haveVolt := readInt(filepath.Join(p, "voltage_now"))
		if en, ok := readInt(filepath.Join(p, "energy_now")); ok {
			ef, _ := readInt(filepath.Join(p, "energy_full"))
			out.EnergyWh += float64(en) / 1e6
			out.EnergyFull += float64(ef) / 1e6
			now += float64(en)
			full += float64(ef)
			out.HasEnergy = true
		} else if ch, ok := readInt(filepath.Join(p, "charge_now")); ok && haveVolt {
			cf, _ := readInt(filepath.Join(p, "charge_full"))
			v := float64(volt) / 1e6
			out.EnergyWh += float64(ch) / 1e6 * v
			out.EnergyFull += float64(cf) / 1e6 * v
			now += float64(ch)
			full += float64(cf)
			out.HasEnergy = true
		}
		if pw, ok := readInt(filepath.Join(p, "power_now")); ok {
			out.PowerWatts += float64(abs(pw)) / 1e6
			out.HasPower = true
		} else if cur, ok := readInt(filepath.Join(p, "current_now")); ok && haveVolt {
			out.PowerWatts += float64(abs(cur)) / 1e6 * float64(volt) / 1e6
			out.HasPower = true
		}
		if c, ok := readInt(filepath.Join(p, "capacity")); ok {
			capSum += float64(c)
		}
	}
	if n == 0 {
		return nil
	}
	switch {
	case full > 0:
		out.Percent = 100 * now / full
	default:
		out.Percent = capSum / float64(n)
	}
	if out.Percent > 100 {
		out.Percent = 100
	}
	if out.Status == "" {
		out.Status = "Unknown"
	}
	return &out
}

func abs(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// Thermal is the hottest sensor reading.
type Thermal struct {
	Celsius float64
	Sensor  string
}

// Temperature returns the hottest plausible thermal-zone reading, falling
// back to CPU hwmon sensors (coretemp, k10temp, zenpower). It returns nil
// when the machine exposes no sensors, which is normal inside VMs.
func (fs FS) Temperature() *Thermal {
	var best *Thermal
	consider := func(milli int64, name string) {
		c := float64(milli) / 1000
		if c <= 0 || c > 150 { // unplugged sensors report 0 or garbage
			return
		}
		if best == nil || c > best.Celsius {
			best = &Thermal{Celsius: c, Sensor: name}
		}
	}
	zones, _ := filepath.Glob(fs.path("class", "thermal", "thermal_zone*"))
	for _, z := range zones {
		if t, ok := readInt(filepath.Join(z, "temp")); ok {
			name, _ := readString(filepath.Join(z, "type"))
			consider(t, name)
		}
	}
	if best != nil {
		return best
	}
	mons, _ := filepath.Glob(fs.path("class", "hwmon", "hwmon*"))
	for _, m := range mons {
		name, _ := readString(filepath.Join(m, "name"))
		switch name {
		case "coretemp", "k10temp", "zenpower", "cpu_thermal", "acpitz":
		default:
			continue
		}
		inputs, _ := filepath.Glob(filepath.Join(m, "temp*_input"))
		for _, in := range inputs {
			if t, ok := readInt(in); ok {
				consider(t, name)
			}
		}
	}
	return best
}

// BlockDevices returns the names under /sys/block: whole disks, never
// partitions. Callers use it to avoid counting a partition and its disk twice.
func (fs FS) BlockDevices() (map[string]bool, error) {
	entries, err := os.ReadDir(fs.path("block"))
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, errors.New("sysfs: /sys/block is empty")
	}
	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out, nil
}
