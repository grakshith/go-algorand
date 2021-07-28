// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package transactions

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/test/framework/fixtures"
)

// this test checks that the txsync outgoing message rate
// varies according to the transaction rate
func TestMessageRateChangesWithTxnRate(t *testing.T) {
	txnRates := []uint{20, 40, 80, 160}
	if testing.Short() {
		txnRates = []uint{20, 40}
	}
	testMessageRateChangesWithTxnRate(t, filepath.Join("nettemplates", "OneNodeTwoRelays.json"), txnRates)
}

func throttleTransactionRate(startTime time.Time, txnRate uint, sentSoFar uint) {
	timeDelta := time.Since(startTime)
	currentTps := float64(sentSoFar) / timeDelta.Seconds()
	if currentTps > float64(txnRate) {
		sleepDuration := float64(sentSoFar)/float64(txnRate) - timeDelta.Seconds()
		sleepTime := time.Duration(int64(math.Round(sleepDuration*1000))) * time.Millisecond
		time.Sleep(sleepTime)
	}
}

func testMessageRateChangesWithTxnRate(t *testing.T, templatePath string, txnRates []uint) {
	t.Parallel()
	a := require.New(fixtures.SynchronizedTest(t))

	var fixture fixtures.RestClientFixture
	fixture.SetupNoStart(t, templatePath)
	configDir, err := fixture.GetNodeDir("Node")
	a.NoError(err)
	cfg, err := config.LoadConfigFromDisk(configDir)
	a.NoError(err)
	cfg.EnableVerbosedTransactionSyncLogging = true
	cfg.SaveToDisk(configDir)
	fixture.Start()

	defer fixture.Shutdown()

	client := fixture.GetLibGoalClientForNamedNode("Node")
	accountsList, err := fixture.GetNodeWalletsSortedByBalance(client.DataDir())
	a.NoError(err)
	account := accountsList[0].Address

	minTxnFee, minAcctBalance, err := fixture.CurrentMinFeeAndBalance()
	a.NoError(err)

	transactionFee := minTxnFee + 5
	amount := minAcctBalance * 3 / 2

	// build the path for the primary node's log file
	nodeDataDir, err := fixture.GetNodeDir("Node")
	a.NoError(err)
	logPath := filepath.Join(nodeDataDir, "node.log")

	// Get the relay's gossip port
	r1, err := fixture.GetNodeController("Relay1")
	a.NoError(err)
	listeningURLRaw, err := r1.GetListeningAddress()
	a.NoError(err)
	listeningURL := strings.Split(listeningURLRaw, "//")[1]

	// seek to `bytesRead` bytes when reading the log file
	bytesRead := 0

	// store message rate for each of the txn rates
	prevMsgRate := 0.0

	for _, txnRate := range txnRates {
		startTime := time.Now()
		txnSentCount := uint(0)

		for {
			// send txns at txnRate for 30s
			timeSinceStart := time.Since(startTime)
			if timeSinceStart > 30*time.Second {
				break
			}

			_, err := client.SendPaymentFromUnencryptedWallet(account, account, transactionFee, amount, GenerateRandomBytes(8))
			a.NoError(err, "fixture should be able to send money")
			txnSentCount++

			throttleTransactionRate(startTime, txnRate, txnSentCount)
		}

		// parse the log to find out message rate and bytes of the log file read
		msgRate, newBytesRead, err := parseLog(logPath, bytesRead, listeningURL)
		a.NoError(err)
		aErrorMessage := fmt.Sprintf("TxSync message rate not monotonic for txn rate: %d", txnRate)
		a.GreaterOrEqual(msgRate, prevMsgRate, aErrorMessage)
		bytesRead += newBytesRead
		prevMsgRate = msgRate
	}

}

func parseLog(logPath string, startByte int, filterAddress string) (float64, int, error) {
	file, err := os.Open(logPath)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	// start reading log file from startByte
	_, err = file.Seek(int64(startByte), 0)
	if err != nil {
		return 0, 0, err
	}

	messageCount := 0
	bytesRead := 0
	var firstTimestamp, lastTimestamp time.Time

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		bytesRead += len(line) + 1
		// look for txnsync messages sent to `filterAddress`
		if strings.Contains(line, "Outgoing Txsync") && strings.Contains(line, filterAddress) {
			// fmt.Println(line)
			var logEvent map[string]interface{}
			json.Unmarshal([]byte(line), &logEvent)
			eventTime := fmt.Sprintf("%v", logEvent["time"])
			message := fmt.Sprintf("%v", logEvent["msg"])
			// skip lines containing empty bloom filter
			if strings.Contains(message, "bloom 0") {
				continue
			}
			// record the timestamps of txnsync messages
			lastTimestamp, err = time.Parse(time.RFC3339, eventTime)
			if err != nil {
				return 0, bytesRead, err
			}
			if firstTimestamp.IsZero() {
				firstTimestamp = lastTimestamp
			}
			messageCount++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, bytesRead, err
	}
	msgRate := float64(messageCount) / (float64(lastTimestamp.Sub(firstTimestamp) / time.Second))
	return msgRate, bytesRead, nil
}
