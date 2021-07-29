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
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/account"
	"github.com/algorand/go-algorand/libgoal"
	"github.com/algorand/go-algorand/test/framework/fixtures"
	"github.com/algorand/go-algorand/util/db"
)

// TODO: remove print statements

// this test checks that the txsync outgoing message rate
// varies according to the transaction rate
func TestMessageRateChangesWithTxnRate(t *testing.T) {
	txnRates := []uint{50, 300, 800, 1300}
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
	nodeDataDir, err := fixture.GetNodeDir("Node")
	a.NoError(err)
	cfg, err := config.LoadConfigFromDisk(nodeDataDir)
	a.NoError(err)
	cfg.EnableVerbosedTransactionSyncLogging = true
	cfg.SaveToDisk(nodeDataDir)
	fixture.Start()

	defer fixture.Shutdown()

	client := fixture.GetLibGoalClientForNamedNode("Node")
	accountsList, err := fixture.GetNodeWalletsSortedByBalance(client.DataDir())
	a.NoError(err)
	account := accountsList[0].Address
	clientAlgod := fixture.GetAlgodClientForController(fixture.GetNodeControllerForDataDir(nodeDataDir))

	_, minAcctBalance, err := fixture.CurrentMinFeeAndBalance()
	a.NoError(err)

	amount := minAcctBalance * 3 / 2

	// get the node account's secret key
	secretKey, err := fetchSecretKey(client, nodeDataDir)
	a.NoError(err)
	signatureSecrets, err := crypto.SecretKeyToSignatureSecrets(secretKey)
	a.NoError(err)

	// build the path for the primary node's log file
	logPath := filepath.Join(nodeDataDir, "node.log")

	// Get the relay's gossip port
	r1, err := fixture.GetNodeController("Relay1")
	a.NoError(err)
	listeningURLRaw, err := r1.GetListeningAddress()
	a.NoError(err)
	listeningURL := strings.Split(listeningURLRaw, "//")[1]

	// // seek to `bytesRead` bytes when reading the log file
	// bytesRead := 0

	// store message rate for each of the txn rates
	prevMsgRate := 0.0

	errChan := make(chan error)
	resetChan := make(chan bool)
	msgRateChan := make(chan float64)
	ctx, stopParsing := context.WithCancel(context.Background())
	defer stopParsing()

	// parseLog continuously monitors the log for txnsync messages
	// resetChan is used to signal it to send results on msgRate chan
	// and reset its internal counters
	// errChan is used to propagate errors if any
	go parseLog(ctx, logPath, listeningURL, errChan, msgRateChan, resetChan)

	for _, txnRate := range txnRates {
		// get the min transaction fee
		minTxnFee, _, err := fixture.CurrentMinFeeAndBalance()
		a.NoError(err)
		transactionFee := minTxnFee * 100

		startTime := time.Now()
		txnSentCount := uint(0)

		for {
			// send txns at txnRate for 30s
			timeSinceStart := time.Since(startTime)
			if timeSinceStart > 30*time.Second {
				break
			}

			tx, err := client.ConstructPayment(account, account, transactionFee, amount, GenerateRandomBytes(8), "", [32]byte{}, 0, 0)
			a.NoError(err)
			signedTxn := tx.Sign(signatureSecrets)

			_, err = clientAlgod.SendRawTransaction(signedTxn)
			a.NoError(err, "Unable to send raw txn")

			// _, err := client.SendPaymentFromUnencryptedWallet(account, account, transactionFee, amount, GenerateRandomBytes(8))
			txnSentCount++

			throttleTransactionRate(startTime, txnRate, txnSentCount)
		}

		endTimeDelta := time.Since(startTime)
		avgTps := float64(txnSentCount) / endTimeDelta.Seconds()
		fmt.Println("Avg TPS: ", avgTps, " Expected: ", txnRate)
		// wait for some time for the logs to get flushed
		time.Sleep(2 * time.Second)
		// fmt.Println("Sent reset")
		// send reset on resetChan to signal the parseLog goroutine to send the msgRate and reset its counters
		resetChan <- true

		select {
		case err := <-errChan:
			a.Error(err)
		case msgRate := <-msgRateChan:
			aErrorMessage := fmt.Sprintf("TxSync message rate not monotonic for txn rate: %d", txnRate)
			a.GreaterOrEqual(msgRate, prevMsgRate, aErrorMessage)
			prevMsgRate = msgRate

		}
		// fmt.Println("Continuing")

		// parse the log to find out message rate and bytes of the log file read
		// msgRate, newBytesRead, err := parseLog(logPath, bytesRead, listeningURL)
		// a.NoError(err)

		// bytesRead += newBytesRead
		// prevMsgRate = msgRate
	}

}

func parseLog(ctx context.Context, logPath string, filterAddress string, errChan chan error, msgRateChan chan float64, resetChan chan bool) {
	file, err := os.Open(logPath)
	if err != nil {
		errChan <- err
		return
	}
	defer file.Close()

	// // start reading log file from startByte
	// _, err = file.Seek(int64(0), 0)
	// if err != nil {
	// 	errChan <- err
	// 	return
	// }

	messageCount := 0
	// bytesRead := 0
	var firstTimestamp, lastTimestamp time.Time

	scanner := bufio.NewScanner(file)
	for {
		select {
		case <-ctx.Done():
			return
		case <-resetChan:
			msgRate := float64(messageCount) / (float64(lastTimestamp.Sub(firstTimestamp) / time.Second))
			msgRateChan <- msgRate
			fmt.Println("Message Rate: ", msgRate, " Message Count: ", messageCount, " Time elapsed: ", lastTimestamp.Sub(firstTimestamp)/time.Second)
			messageCount = 0
			firstTimestamp = time.Time{}
			lastTimestamp = time.Time{}
			continue
		default:
		}
		scanned := scanner.Scan()
		if !scanned {
			if err := scanner.Err(); err != nil {
				errChan <- err
				return
			}
			time.Sleep(100 * time.Millisecond)
			scanner = bufio.NewScanner(file)
			continue
		}

		line := scanner.Text()
		// bytesRead += len(line) + 1
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
				errChan <- err
				return
			}
			if firstTimestamp.IsZero() {
				firstTimestamp = lastTimestamp
			}
			messageCount++
		}
	}
}

func fetchSecretKey(client libgoal.Client, dataDir string) (crypto.PrivateKey, error) {
	secretKey := crypto.PrivateKey{}
	genID, err := client.GenesisID()
	if err != nil {
		return secretKey, err
	}

	keyDir := filepath.Join(dataDir, genID)
	files, err := ioutil.ReadDir(keyDir)
	if err != nil {
		return secretKey, err
	}

	// For each of these files
	for _, info := range files {
		var handle db.Accessor

		filename := info.Name()

		// If it isn't a key file we care about, skip it
		if config.IsRootKeyFilename(filename) {
			handle, err = db.MakeAccessor(filepath.Join(keyDir, filename), true, false)
			if err != nil {
				// Couldn't open it, skip it
				continue
			}

			// Fetch an account.Root from the database
			root, err := account.RestoreRoot(handle)
			if err != nil {
				return secretKey, err
			}

			secretKey = crypto.PrivateKey(root.Secrets().SK)
			break
		}

	}

	return secretKey, nil
}
