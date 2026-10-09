package gpu

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	appleIoregCmd = "ioreg -r -d 1 -w 0 -c AGXAccelerator"
	// hw.memsize of the M3 Max the fixture was captured on.
	appleTotalRAM = 38654705664
)

func ioregFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("testdata/ioreg-agx-darwin.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseAppleStats(t *testing.T) {
	got, ok := parseAppleStats([]byte(ioregFixture(t)), appleTotalRAM, 1789000000)
	want := GPUSample{GPUID: 0, Utilization: 30, VRAMUsedMB: 2613116928 / 1024 / 1024, VRAMTotalMB: appleTotalRAM / 1024 / 1024, Timestamp: 1789000000}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Errorf("parseAppleStats = %+v, %v\nwant %+v", got, ok, want)
	}
}

// "In use system memory (driver)" comes first in the capture and shares the
// real key's prefix: a search for the key without its closing quote reads
// the driver's figure. The keys are undocumented, so reordering is tolerated.
func TestParseAppleStatsKeyPrefixTrap(t *testing.T) {
	stats := `      "PerformanceStatistics" = {"Device Utilization %"=12,"In use system memory (driver)"=999999999,"Some Future Key"={"a"=1,"b"=2},"In use system memory"=1048576}`
	got, ok := parseAppleStats([]byte(stats), appleTotalRAM, 1)
	if !ok || got.VRAMUsedMB != 1 || got.Utilization != 12 {
		t.Errorf("got %+v, %v; want 1 MiB in use at 12%%", got, ok)
	}
}

// A missing key is "cannot say", never a zero reading.
func TestParseAppleStatsMissing(t *testing.T) {
	fixture := ioregFixture(t)
	for name, in := range map[string]string{
		"no utilisation": strings.Replace(fixture, `"Device Utilization %"=30,`, "", 1),
		"no memory":      strings.Replace(fixture, `,"In use system memory"=2613116928`, "", 1),
		"no statistics":  strings.Replace(fixture, `"PerformanceStatistics"`, `"Other"`, 1),
		"empty":          "",
		"bad number":     strings.Replace(fixture, `"Device Utilization %"=30`, `"Device Utilization %"=abc`, 1),
	} {
		if got, ok := parseAppleStats([]byte(in), appleTotalRAM, 1); ok {
			t.Errorf("%s: got %+v, want no sample", name, got)
		}
	}
	if got, ok := parseAppleStats([]byte(fixture), 0, 1); ok {
		t.Errorf("unknown total RAM: got %+v, want no sample", got)
	}
}

func TestAppleCollector(t *testing.T) {
	h := NewHistory(10)
	c := NewAppleCollector(h, NewBroadcaster(), fakeRunner(map[string]string{appleIoregCmd: ioregFixture(t)}), func() int64 { return appleTotalRAM })
	if !c.Available() || c.PowerAvailable() {
		t.Fatalf("available=%v powerAvailable=%v, want true/false", c.Available(), c.PowerAvailable())
	}
	c.Sample(context.Background())
	latest := h.Latest()
	if len(latest) != 1 || latest[0].GPUID != 0 || latest[0].Utilization != 30 {
		t.Errorf("history = %+v, want one card at 30%%", latest)
	}
}

func TestAppleCollectorUnavailable(t *testing.T) {
	c := NewAppleCollector(NewHistory(10), NewBroadcaster(), fakeRunner(nil), func() int64 { return appleTotalRAM })
	if c.Available() || c.PowerAvailable() {
		t.Errorf("available=%v powerAvailable=%v, want false/false without ioreg", c.Available(), c.PowerAvailable())
	}
	c.Sample(context.Background()) // must not run anything or panic
}
