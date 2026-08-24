package metrics

import (
	"io/fs"
	"path"
	"sort"
	"strings"
)

// readSensors collects hardware readings from /sys.
//
// Entries under /sys/class/hwmon are symlinks into the device tree, so the
// scan never filters on IsDir; it just tries to read the files it wants and
// skips whatever is absent. Drivers publish wildly different subsets, and
// treating a missing file as "this chip does not report that" is the only
// approach that works across them.
func readSensors(sysfs fs.FS) ([]Sensor, error) {
	var out []Sensor

	ents, err := fs.ReadDir(sysfs, "class/hwmon")
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		dir := path.Join("class/hwmon", e.Name())
		chip, _ := readTrimmed(sysfs, path.Join(dir, "name"))
		if chip == "" {
			chip = e.Name()
		}
		out = append(out, readHwmonDir(sysfs, dir, chip)...)
	}

	out = append(out, readBatteries(sysfs)...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

// readHwmonDir reads the temperature and fan channels of one hwmon chip.
func readHwmonDir(sysfs fs.FS, dir, chip string) []Sensor {
	files, err := fs.ReadDir(sysfs, dir)
	if err != nil {
		return nil
	}
	var out []Sensor
	for _, f := range files {
		name := f.Name()
		base, ok := strings.CutSuffix(name, "_input")
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(base, "temp"):
			if s, ok := readChannel(sysfs, dir, base, chip, SensorTemp, 1000); ok {
				out = append(out, s)
			}
		case strings.HasPrefix(base, "fan"):
			if s, ok := readChannel(sysfs, dir, base, chip, SensorFan, 1); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// readChannel reads one numbered channel of a chip. divisor converts the
// kernel's integer unit to the display unit: temperatures are published in
// millidegrees, fan speeds already in RPM.
func readChannel(sysfs fs.FS, dir, base, chip string, kind SensorKind, divisor float64) (Sensor, bool) {
	raw, err := readTrimmed(sysfs, path.Join(dir, base+"_input"))
	if err != nil {
		return Sensor{}, false
	}
	s := Sensor{Kind: kind, Chip: chip, Value: atof(raw) / divisor}

	// A channel without a label is identified by the chip and channel number,
	// which is still more use on screen than "temp1" alone.
	if label, err := readTrimmed(sysfs, path.Join(dir, base+"_label")); err == nil && label != "" {
		s.Label = label
	} else {
		s.Label = chip + " " + base
	}
	if v, err := readTrimmed(sysfs, path.Join(dir, base+"_max")); err == nil {
		s.High = atof(v) / divisor
	}
	if v, err := readTrimmed(sysfs, path.Join(dir, base+"_crit")); err == nil {
		s.Crit = atof(v) / divisor
	}
	return s, true
}

// readBatteries reads charge percentages from /sys/class/power_supply.
func readBatteries(sysfs fs.FS) []Sensor {
	ents, err := fs.ReadDir(sysfs, "class/power_supply")
	if err != nil {
		return nil
	}
	var out []Sensor
	for _, e := range ents {
		dir := path.Join("class/power_supply", e.Name())
		// Mains adapters live here too and have no capacity file.
		if t, _ := readTrimmed(sysfs, path.Join(dir, "type")); t != "Battery" {
			continue
		}
		cap, err := readTrimmed(sysfs, path.Join(dir, "capacity"))
		if err != nil {
			continue
		}
		status, _ := readTrimmed(sysfs, path.Join(dir, "status"))
		label := e.Name()
		if status != "" {
			label += " (" + strings.ToLower(status) + ")"
		}
		out = append(out, Sensor{Kind: SensorBattery, Chip: e.Name(), Label: label, Value: atof(cap)})
	}
	return out
}
