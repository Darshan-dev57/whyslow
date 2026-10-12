package sysfs

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, root, rel, val string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(val), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Broken or half-populated sysfs trees must give nil or sane values, never
// a panic or a percentage outside 0..100.
func TestBrokenSysfs(t *testing.T) {
	cases := map[string]map[string]string{
		"garbage battery": {
			"class/power_supply/BAT0/type":       "Battery\n",
			"class/power_supply/BAT0/capacity":   "lots\n",
			"class/power_supply/BAT0/status":     "\n",
			"class/power_supply/BAT0/energy_now": "-5\n",
			"class/power_supply/BAT0/power_now":  "x\n",
		},
		"battery with only a type": {
			"class/power_supply/BAT1/type": "Battery\n",
		},
		"capacity over 100": {
			"class/power_supply/BAT0/type":     "Battery\n",
			"class/power_supply/BAT0/capacity": "250\n",
			"class/power_supply/BAT0/status":   "Discharging\n",
		},
		"energy above full": {
			"class/power_supply/BAT0/type":        "Battery\n",
			"class/power_supply/BAT0/energy_now":  "90000000\n",
			"class/power_supply/BAT0/energy_full": "45000000\n",
			"class/power_supply/BAT0/status":      "Discharging\n",
		},
		"thermal zone without temp": {
			"class/thermal/thermal_zone0/type": "x86_pkg_temp\n",
		},
		"thermal garbage": {
			"class/thermal/thermal_zone0/temp": "N/A\n",
			"class/thermal/thermal_zone1/temp": "-5000\n",
			"class/thermal/thermal_zone2/temp": "999999999\n",
			"class/hwmon/hwmon0/name":          "coretemp\n",
			"class/hwmon/hwmon0/temp1_input":   "\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			for rel, val := range files {
				write(t, root, rel, val)
			}
			fs := New(root)
			if b := fs.Battery(); b != nil && (b.Percent < 0 || b.Percent > 100) {
				t.Errorf("battery percent %v", b.Percent)
			}
			if th := fs.Temperature(); th != nil {
				t.Errorf("temperature from garbage: %+v", th)
			}
		})
	}
}
