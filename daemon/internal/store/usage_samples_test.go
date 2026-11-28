package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageSamplesKeepOrderSkipTheUnmovedAndTheOlder(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "mesh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	t0 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	put := func(at time.Time, used float64) {
		if err := st.Write(ctx, nil, "", func(tx *Tx) error {
			return tx.PutUsageSample(UsageSample{Pool: "p", At: at, Used: used, Resets: t0.Add(24 * time.Hour)}, 10*time.Minute, 45*24*time.Hour)
		}); err != nil {
			t.Fatal(err)
		}
	}
	put(t0, 10)
	put(t0.Add(2*time.Minute), 10)  // nothing moved within ten minutes: not kept
	put(t0.Add(3*time.Minute), 12)  // moved: kept
	put(t0.Add(-1*time.Hour), 5)    // older than the last reading: not kept
	put(t0.Add(30*time.Minute), 10) // kept
	got, err := st.Read(ctx).UsageSamples("p", t0.Add(-2*time.Hour))
	if err != nil || len(got) != 3 || got[0].Used != 10 || got[1].Used != 12 || got[2].Used != 10 {
		t.Fatalf("samples %v, %v", got, err)
	}
	// 45 days on, the first ones are dropped.
	put(t0.Add(46*24*time.Hour), 1)
	if got, _ := st.Read(ctx).UsageSamples("p", t0.Add(-2*time.Hour)); len(got) != 1 {
		t.Errorf("old samples kept: %v", got)
	}
	if pools, _ := st.Read(ctx).UsagePools(); len(pools) != 1 || pools[0] != "p" {
		t.Errorf("pools %v", pools)
	}
}
