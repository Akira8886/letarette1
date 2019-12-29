package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/nats-io/nats.go"

	"github.com/erkkah/letarette/pkg/client"
	"github.com/erkkah/letarette/pkg/logger"
	"github.com/erkkah/letarette/pkg/protocol"
)

type testSet struct {
	Iterations int
	Spaces     []string
	Queries    []string
	Limit      int
	Offset     int
}

type testResult struct {
	Start    time.Time
	End      time.Time
	Duration float32
	Status   protocol.SearchStatusCode
	Err      error
}

var cmdline struct {
	Agent bool
	Run   bool

	TestSet string `docopt:"<testset.json>"`

	NATSURL string `docopt:"-n"`
	Output  string `docopt:"-o"`
}

func main() {
	usage := `Letarette load generator

Usage:
    load agent [-n <natsURL>]
    load run [-n <natsURL>] [-o <file>] <testset.json>

Options:
    -n <natsURL> NATS server URL [default: localhost]
    -o <file>    Write raw CSV data to <file>
`

	args, err := docopt.ParseDoc(usage)
	if err != nil {
		logger.Error.Printf("Failed to parse args: %v", err)
		return
	}

	err = args.Bind(&cmdline)
	if err != nil {
		logger.Error.Printf("Failed to bind args: %v", err)
		return
	}

	if cmdline.Agent {
		err := startAgent()
		if err != nil {
			logger.Error.Printf("Failed to start load agent: %v", err)
			return
		}
		logger.Info.Printf("Agent waiting for load requests")
		select {}
	} else if cmdline.Run {
		testSet, err := loadTestSet(cmdline.TestSet)
		if err != nil {
			logger.Error.Printf("Failed to load test set: %v", err)
			return
		}

		runTestSet(testSet)
	} else {
		docopt.PrintHelpAndExit(nil, usage)
	}
}

// NATSConnect connects to NATS :)
func NATSConnect() (*nats.EncodedConn, error) {
	natsOptions := []nats.Option{
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Millisecond * 500),
	}

	nc, err := nats.Connect(cmdline.NATSURL, natsOptions...)
	if err != nil {
		return nil, err
	}
	ec, err := nats.NewEncodedConn(nc, nats.JSON_ENCODER)
	if err != nil {
		return nil, err
	}

	return ec, nil
}

func startAgent() error {
	agent, err := client.NewSearchAgent([]string{cmdline.NATSURL}, client.WithTimeout(time.Second*10))
	if err != nil {
		return err
	}

	ec, err := NATSConnect()
	if err != nil {
		return err
	}

	_, err = ec.Subscribe("leta.load.ping", func(interface{}) {
		clientID, _ := ec.Conn.GetClientID()
		ec.Publish("leta.load.pong", &clientID)
	})
	if err != nil {
		return err
	}

	_, err = ec.Subscribe("leta.load.request", func(set *testSet) {
		logger.Info.Printf("Running load request")
		results := make([]testResult, set.Iterations)
		for i := 0; i < set.Iterations; i++ {
			q := set.Queries[rand.Intn(len(set.Queries))]
			start := time.Now()
			res, err := agent.Search(q, set.Spaces, set.Limit, set.Offset)
			results[i] = testResult{
				Start:    start,
				End:      time.Now(),
				Duration: res.Duration,
				Status:   res.Status,
				Err:      err,
			}
		}
		ec.Publish("leta.load.response", &results)
	})
	if err != nil {
		return err
	}

	return nil
}

func runTestSet(set testSet) error {
	ec, err := NATSConnect()
	if err != nil {
		return err
	}

	var agents int32
	pingSub, err := ec.Subscribe("leta.load.pong", func(agent *int64) {
		logger.Debug.Printf("Got response from agent %v", *agent)
		atomic.AddInt32(&agents, 1)
	})
	if err != nil {
		return err
	}

	ec.Publish("leta.load.ping", nil)

	select {
	case <-time.After(time.Second * 2):
		pingSub.Unsubscribe()
	}

	rand.Seed(time.Now().Unix())

	var wg sync.WaitGroup
	wg.Add(int(agents) + 1)

	resultChannel := make(chan []testResult, 10)
	results := make([]testResult, 0, int(agents))
	go func() {
		for result := range resultChannel {
			results = append(results, result...)
			logger.Debug.Printf("Adding result")
			if len(results) == int(agents)*set.Iterations {
				logger.Debug.Printf("All done")
				wg.Done()
				break
			}
		}
	}()
	responseSub, err := ec.Subscribe("leta.load.response", func(result *[]testResult) {
		logger.Debug.Printf("Got response with %v results", len(*result))
		resultChannel <- *result
		wg.Done()
	})
	if err != nil {
		return err
	}
	responseSub.AutoUnsubscribe(int(agents))

	start := time.Now()
	ec.Publish("leta.load.request", &set)

	logger.Debug.Printf("Waiting...")
	wg.Wait()
	end := time.Now()

	logger.Debug.Printf("Reporting...")
	report(results, int(agents), end.Sub(start))
	return nil
}

func report(results []testResult, clients int, total time.Duration) {
	if cmdline.Output != "" {
		output, err := os.Create(cmdline.Output)
		if err != nil {
			logger.Error.Printf("Failed to create output file: %v", err)
			return
		}
		defer output.Close()
		for _, res := range results {
			var status = res.Status.String()
			if res.Err != nil {
				status = fmt.Sprintf("%v", res.Err)
			}
			realDuration := res.End.Sub(res.Start)
			fmt.Fprintf(output, "%v,%v,%q\n", realDuration.Seconds(), res.Duration, status)
		}
	}

	var durationMean float32
	var totalMean float64

	for _, res := range results {
		durationMean += res.Duration
		totalMean += res.End.Sub(res.Start).Seconds()
	}
	durationMean /= float32(len(results))
	totalMean /= float64(len(results))

	sort.Slice(results, func(i, j int) bool {
		return results[i].Duration < results[j].Duration
	})

	durationMedian := results[len(results)/2].Duration
	duration90 := results[int(float32(len(results))*0.9)].Duration
	duration95 := results[int(float32(len(results))*0.95)].Duration
	duration99 := results[int(float32(len(results))*0.99)].Duration

	sort.Slice(results, func(i, j int) bool {
		totalA := results[i].End.Sub(results[i].Start).Seconds()
		totalB := results[j].End.Sub(results[j].Start).Seconds()
		return totalA < totalB
	})

	totalMedian := results[len(results)/2].Duration
	total90 := results[int(float32(len(results))*0.9)].Duration
	total95 := results[int(float32(len(results))*0.95)].Duration
	total99 := results[int(float32(len(results))*0.99)].Duration

	fmt.Printf("Test set processed by %v agents in %.2fs\n", clients, total.Seconds())

	fmt.Printf("\nQuery processing times:\n")
	fmt.Printf("Mean:\t%v\nMedian:\t%v\n", durationMean, durationMedian)
	fmt.Printf("90%%:\t%v\n", duration90)
	fmt.Printf("95%%:\t%v\n", duration95)
	fmt.Printf("99%%:\t%v\n", duration99)

	fmt.Printf("\nTotal roundtrip times:\n")
	fmt.Printf("Mean:\t%v\nMedian:\t%v\n", float32(totalMean), totalMedian)
	fmt.Printf("90%%:\t%v\n", total90)
	fmt.Printf("95%%:\t%v\n", total95)
	fmt.Printf("99%%:\t%v\n", total99)

}

func loadTestSet(path string) (testSet, error) {
	file, err := os.Open(path)
	if err != nil {
		return testSet{}, err
	}

	decoder := json.NewDecoder(file)
	var loaded testSet
	err = decoder.Decode(&loaded)
	return loaded, err
}
