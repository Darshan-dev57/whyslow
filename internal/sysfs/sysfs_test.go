package sysfs

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestBatteryEnergyBased(t *testing.T) {
	b := New("testdata/laptop").Battery()
	if b == nil {
		t.Fatal("expected a battery")
	}
	if len(b.Names) != 1 || b.Names[0] != "BAT0" {
		t.Errorf("peripheral battery not skipped: %v", b.Names)
	}
	if !b.Discharging() || !b.HasEnergy || !b.HasPower {
		t.Errorf("flags: %+v", b)
	}
	if !near(b.Percent, 77) || !near(b.EnergyWh, 38.5) || !near(b.EnergyFull, 50) || !near(b.PowerWatts, 9.12) {
		t.Errorf("values: %+v", b)
	}
}

func TestBatteryChargeBased(t *testing.T) {
	b := New("testdata/laptop2").Battery()
	if b == nil {
		t.Fatal("expected a battery")
	}
	if b.Discharging() || b.Status != "Charging" {
		t.Errorf("status %q", b.Status)
	}
	// 2 Ah at 12 V = 24 Wh; |-1.5 A| * 12 V = 18 W.
	if !near(b.EnergyWh, 24) || !near(b.PowerWatts, 18) || !near(b.Percent, 50) {
		t.Errorf("values: %+v", b)
	}
}

func TestNoBattery(t *testing.T) {
	if b := New("testdata/vm").Battery(); b != nil {
		t.Errorf("VM should have no battery, got %+v", b)
	}
	if New("testdata/missing").Battery().Discharging() {
		t.Error("nil battery must not be discharging")
	}
}

func TestTemperature(t *testing.T) {
	th := New("testdata/laptop").Temperature()
	if th == nil || !near(th.Celsius, 61.5) || th.Sensor != "x86_pkg_temp" {
		t.Errorf("thermal zones: %+v", th)
	}
	th = New("testdata/laptop2").Temperature()
	if th == nil || !near(th.Celsius, 55.25) || th.Sensor != "k10temp" {
		t.Errorf("hwmon fallback (nvme must be ignored): %+v", th)
	}
	if th := New("testdata/vm").Temperature(); th != nil {
		t.Errorf("VM should have no sensor, got %+v", th)
	}
}

func TestBlockDevices(t *testing.T) {
	d, err := New("testdata/vm").BlockDevices()
	if err != nil {
		t.Fatal(err)
	}
	if !d["vda"] || !d["loop0"] || len(d) != 2 {
		t.Errorf("got %v", d)
	}
	if _, err := New("testdata/missing").BlockDevices(); err == nil {
		t.Error("missing /sys/block should fail")
	}
}
