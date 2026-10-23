package localruntime

import (
	"math"
	"runtime"
	"sync"
	"testing"
)

func TestDefaultResourceSamplerConcurrentSamples(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("Linux resource sampling uses /proc")
	}
	sampler := &defaultResourceSampler{}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			<-start
			for range 3 {
				sample, err := sampler.Sample()
				if err != nil {
					t.Errorf("Sample(): %v", err)
					return
				}
				if math.IsNaN(sample.CPUPercent) || sample.CPUPercent < 0 || sample.CPUPercent > 100 {
					t.Errorf("CPU percent = %v, want a valid counter delta", sample.CPUPercent)
				}
			}
		})
	}
	close(start)
	workers.Wait()
}
