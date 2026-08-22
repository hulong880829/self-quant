package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
)

type message struct {
	Channel string `json:"channel"`
	WallNS  string `json:"wall_ns"`
}

func percentile(values []int64, p float64) float64 {
	index := int(math.Floor(p * float64(len(values)-1)))
	return float64(values[index]) / float64(time.Millisecond)
}

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: fairprice_latency WS_URL PROFILE SYMBOL SECONDS")
		os.Exit(2)
	}
	duration, err := strconv.Atoi(os.Args[4])
	if err != nil || duration <= 0 {
		fmt.Fprintln(os.Stderr, "SECONDS must be positive")
		os.Exit(2)
	}
	url := os.Args[1]
	if token := os.Getenv("AGGDATA_BROWSER_TOKEN"); token != "" {
		separator := "?"
		for _, value := range url {
			if value == '?' {
				separator = "&"
				break
			}
		}
		url += separator + "token=" + token
	}
	headers := http.Header{}
	if origin := os.Getenv("FAIRPRICE_ORIGIN"); origin != "" {
		headers.Set("Origin", origin)
	}
	connection, _, err := websocket.DefaultDialer.Dial(url, headers)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{
		"op": "subscribe", "profile": os.Args[2], "symbol": os.Args[3],
		"channel": "fairprice", "depth": 0,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	deadline := time.Now().Add(time.Duration(duration) * time.Second)
	_ = connection.SetReadDeadline(deadline)
	lags := make([]int64, 0, duration*20)
	for time.Now().Before(deadline) {
		_, payload, err := connection.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err) {
				fmt.Fprintln(os.Stderr, err)
			}
			break
		}
		received := time.Now().UnixNano()
		var value message
		if json.Unmarshal(payload, &value) != nil || value.Channel != "fairprice" {
			continue
		}
		wallNS, err := strconv.ParseInt(value.WallNS, 10, 64)
		if err == nil && received >= wallNS {
			lags = append(lags, received-wallNS)
		}
	}
	if len(lags) == 0 {
		fmt.Fprintln(os.Stderr, "no fairprice samples received")
		os.Exit(1)
	}
	sort.Slice(lags, func(i, j int) bool { return lags[i] < lags[j] })
	fmt.Printf("samples=%d p50_ms=%.3f p95_ms=%.3f p99_ms=%.3f max_ms=%.3f\n",
		len(lags), percentile(lags, .50), percentile(lags, .95),
		percentile(lags, .99), float64(lags[len(lags)-1])/float64(time.Millisecond))
}
