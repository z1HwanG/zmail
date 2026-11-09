package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func BenchmarkSQLiteReadConcurrency(b *testing.B) {
	for _, maxConns := range []int{1, 4} {
		b.Run(fmt.Sprintf("connections-%d", maxConns), func(b *testing.B) {
			dir := b.TempDir()
			st, err := Open(filepath.Join(dir, "bench.db"), filepath.Join(dir, "raw"))
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			st.DB().SetMaxOpenConns(maxConns)
			st.DB().SetMaxIdleConns(maxConns)
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					var count int
					if err := st.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

func TestSQLiteConcurrentReadSmoke(t *testing.T) {
	st := openTestStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				var count int
				if err := st.DB().QueryRowContext(context.Background(), "SELECT count(*) FROM jobs").Scan(&count); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
