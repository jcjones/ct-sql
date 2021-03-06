/* This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at http://mozilla.org/MPL/2.0/. */

// Based on github.com/jmhodges/certificatetransparency/tools/lecsv

package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"golang.org/x/net/context"

	_ "github.com/go-sql-driver/mysql"

	"github.com/go-gorp/gorp"
	"github.com/google/certificate-transparency/go"
	"github.com/google/certificate-transparency/go/client"
	"github.com/google/certificate-transparency/go/jsonclient"
	"github.com/jcjones/ct-sql/censysdata"
	"github.com/jcjones/ct-sql/sqldb"
	"github.com/jcjones/ct-sql/utils"
	"github.com/jpillora/backoff"
)

var (
	config = utils.NewCTConfig()
)

type CtLogEntry struct {
	LogEntry *ct.LogEntry
	LogID    int
}

type LogDownloader struct {
	Database            *sqldb.EntriesDatabase
	EntryChan           chan CtLogEntry
	Display             *utils.ProgressDisplay
	ThreadWaitGroup     *sync.WaitGroup
	DownloaderWaitGroup *sync.WaitGroup
	Backoff             *backoff.Backoff
}

func NewLogDownloader(db *sqldb.EntriesDatabase) *LogDownloader {
	return &LogDownloader{
		Database:            db,
		EntryChan:           make(chan CtLogEntry),
		Display:             utils.NewProgressDisplay(),
		ThreadWaitGroup:     new(sync.WaitGroup),
		DownloaderWaitGroup: new(sync.WaitGroup),
		Backoff: &backoff.Backoff{
			Min:    10 * time.Millisecond,
			Max:    1 * time.Second,
			Jitter: true,
		},
	}
}

func (ld *LogDownloader) StartThreads() {
	numWorkers := *config.NumThreads * runtime.NumCPU()
	for i := 0; i < numWorkers; i++ {
		go ld.insertCTWorker()
	}
}

func (ld *LogDownloader) Stop() {
	close(ld.EntryChan)
	ld.Display.Close()
}

func (ld *LogDownloader) Download(ctLogUrl string) {
	if *config.OffsetByte > 0 {
		log.Printf("[%s] Cannot set offsetByte for CT log downloads", ctLogUrl)
		return
	}

	ctLog, err := client.New(ctLogUrl, nil, jsonclient.Options{})
	if err != nil {
		log.Printf("[%s] Unable to construct CT log client: %s", ctLogUrl, err)
		return
	}

	log.Printf("[%s] Fetching signed tree head... ", ctLogUrl)
	sth, err := ctLog.GetSTH(context.Background())
	if err != nil {
		log.Printf("[%s] Unable to fetch signed tree head: %s", ctLogUrl, err)
		return
	}

	// Set pointer in DB, now that we've verified the log works
	urlParts, err := url.Parse(ctLogUrl)
	if err != nil {
		log.Printf("[%s] Unable to parse Certificate Log: %s", ctLogUrl, err)
		return
	}
	logObj, err := ld.Database.GetLogState(fmt.Sprintf("%s%s", urlParts.Host, urlParts.Path))
	if err != nil {
		log.Printf("[%s] Unable to set Certificate Log: %s", ctLogUrl, err)
		return
	}

	var origCount uint64
	// Now we're OK to use the DB
	if *config.Offset > 0 {
		log.Printf("[%s] Starting from offset %d", ctLogUrl, *config.Offset)
		origCount = *config.Offset
	} else {
		log.Printf("[%s] Counting existing entries... ", ctLogUrl)
		origCount = logObj.MaxEntry
		if err != nil {
			log.Printf("[%s] Failed to read entries file: %s", ctLogUrl, err)
			return
		}
	}

	log.Printf("[%s] %d total entries at %s\n", ctLogUrl, sth.TreeSize, utils.Uint64ToTimestamp(sth.Timestamp).Format(time.ANSIC))
	if origCount == sth.TreeSize {
		log.Printf("[%s] Nothing to do\n", ctLogUrl)
		return
	}

	endPos := sth.TreeSize
	if *config.Limit > 0 && endPos > origCount+*config.Limit {
		endPos = origCount + *config.Limit
	}

	log.Printf("[%s] Going from %d to %d\n", ctLogUrl, origCount, endPos)

	finalIndex, finalTime, err := ld.DownloadCTRangeToChannel(logObj.LogID, ctLog, origCount, endPos)
	if err != nil {
		log.Printf("\n[%s] Download halting, error caught: %s\n", ctLogUrl, err)
	}

	logObj.MaxEntry = finalIndex
	if finalTime != 0 {
		logObj.LastEntryTime = utils.Uint64ToTimestamp(finalTime)
	}

	log.Printf("[%s] Saved state. MaxEntry=%d, LastEntryTime=%s", ctLogUrl, logObj.MaxEntry, logObj.LastEntryTime)
	ld.Database.SaveLogState(logObj)
}

// DownloadRange downloads log entries from the given starting index till one
// less than upTo. If status is not nil then status updates will be written to
// it until the function is complete, when it will be closed. The log entries
// are provided to an output channel.
func (ld *LogDownloader) DownloadCTRangeToChannel(logID int, ctLog *client.LogClient, start, upTo uint64) (uint64, uint64, error) {
	if ld.EntryChan == nil {
		return start, 0, fmt.Errorf("No output channel provided")
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(sigChan)
	defer close(sigChan)

	progressTicker := time.NewTicker(10 * time.Second)
	defer progressTicker.Stop()

	var lastTime uint64

	index := start
	for index < upTo {
		max := index + 1024
		if max >= upTo {
			max = upTo - 1
		}
		rawEnts, err := ctLog.GetEntries(int64(index), int64(max))
		if err != nil {
			return index, lastTime, err
		}

		for arrayOffset := 0; arrayOffset < len(rawEnts); {
			ent := rawEnts[arrayOffset]
			// Are there waiting signals?
			select {
			case sig := <-sigChan:
				return index, lastTime, fmt.Errorf("Signal caught: %s", sig)
			case ld.EntryChan <- CtLogEntry{&ent, logID}:
				lastTime = ent.Leaf.TimestampedEntry.Timestamp
				if uint64(ent.Index) != index {
					return index, lastTime, fmt.Errorf("Index mismatch, local: %v, remote: %v", index, ent.Index)
				}

				index++
				arrayOffset++
				ld.Backoff.Reset()
			case <-progressTicker.C:
				ld.Display.UpdateProgress(fmt.Sprintf("%d", logID), start, index, upTo)
			default:
				// Channel full, retry
				time.Sleep(ld.Backoff.Duration())
			}
		}
	}

	return index, lastTime, nil
}

func (ld *LogDownloader) insertCTWorker() {
	ld.ThreadWaitGroup.Add(1)
	defer ld.ThreadWaitGroup.Done()
	for ep := range ld.EntryChan {
		err := ld.Database.InsertCTEntry(ep.LogEntry, ep.LogID)
		if err != nil {
			log.Printf("Problem inserting certificate: index: %d log: %s error: %s", ep.LogEntry.Index, *config.LogUrl, err)
		}
	}
}

func processImporter(importer censysdata.Importer, db *sqldb.EntriesDatabase, wg *sync.WaitGroup) error {
	entryChan := make(chan censysdata.CensysEntry)
	defer close(entryChan)
	wg.Add(1)
	defer wg.Done()

	display := utils.NewProgressDisplay()
	display.StartDisplay(wg)
	defer display.Close()

	numWorkers := *config.NumThreads * runtime.NumCPU()
	for i := 0; i < numWorkers; i++ {
		go insertCensysWorker(entryChan, db, wg)
	}

	startOffset := *config.OffsetByte

	//
	// Fast forward
	//
	if *config.Offset > 0 && *config.OffsetByte > 0 {
		return fmt.Errorf("You may not set both offset and offsetByte")
	}

	if *config.Offset > 0 {
		err := importer.SeekLine(*config.Offset)
		if err != nil {
			return err
		}
	}

	if *config.OffsetByte > 0 {
		err := importer.SeekByte(*config.OffsetByte)
		if err != nil {
			return err
		}
	}

	var maxOffset uint64

	log.Printf("Starting import from offset=%d, line_limit=%d", importer.ByteOffset(), *config.Limit)

	// We've already fast-forwarded, so start at 0.
	for count := uint64(0); ; count++ {
		if *config.Limit > uint64(0) && count >= *config.Limit {
			return nil
		}
		ent, err := importer.NextEntry()
		if err != nil || ent == nil {
			return err
		}
		if count%128 == 0 {
			// Lazily obtain the max offset
			if maxOffset < 1 {
				maxOffset, err = importer.Size()
				if err != nil {
					return err
				}
			}

			display.UpdateProgress("importer", startOffset, ent.Offset, maxOffset)
		}
		entryChan <- *ent
	}

	return nil
}

func insertCensysWorker(entries <-chan censysdata.CensysEntry, db *sqldb.EntriesDatabase, wg *sync.WaitGroup) {
	wg.Add(1)
	defer wg.Done()
	for ep := range entries {
		if ep.Valid_nss {
			err := db.InsertCensysEntry(&ep)
			if err != nil {
				log.Printf("Problem inserting certificate: index: %d error: %s", ep.Offset, err)
			}
		}
	}
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("")
	dbConnectStr, err := sqldb.RecombineURLForDB(*config.DbConnect)
	if err != nil {
		log.Printf("unable to parse %s: %s", *config.DbConnect, err)
	}

	if len(dbConnectStr) == 0 || (config.CensysPath == nil && config.LogUrl == nil) {
		config.Usage()
		os.Exit(2)
	}

	db, err := sql.Open("mysql", dbConnectStr)
	if err != nil {
		log.Fatalf("unable to open SQL: %s: %s", dbConnectStr, err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("unable to ping SQL: %s: %s", dbConnectStr, err)
	}

	var certFolderDB *utils.FolderDatabase
	if config.CertPath != nil && len(*config.CertPath) > 0 {
		certFolderDB, err = utils.NewFolderDatabase(*config.CertPath, 0444, *config.CertsPerFolder)
		if err != nil {
			log.Fatalf("unable to open Certificate Path: %s: %s", config.CertPath, err)
		}
	}

	var issuerCNList []string
	if config.IssuerCNFilter != nil && len(*config.IssuerCNFilter) > 0 {
		for _, part := range strings.Split(*config.IssuerCNFilter, ",") {
			cnFilter := strings.TrimSpace(part)
			if len(cnFilter) > 0 {
				issuerCNList = append(issuerCNList, cnFilter)
			}
		}
	}

	var earliestDate time.Time
	if config.EarliestDateFilter != nil && len(*config.EarliestDateFilter) > 1 {
		earliestDate, err = time.Parse("2006-01-02", *config.EarliestDateFilter)
		if err != nil {
			log.Fatalf("unable to parse EarliestDateFilter: %s: %s", *config.EarliestDateFilter, err)
		}
	}

	dialect := gorp.MySQLDialect{Engine: "InnoDB", Encoding: "UTF8"}
	dbMap := &gorp.DbMap{Db: db, Dialect: dialect}
	entriesDb := &sqldb.EntriesDatabase{
		DbMap:               dbMap,
		SQLDebug:            *config.SQLDebug,
		Verbose:             *config.Verbose,
		FullCerts:           certFolderDB,
		KnownIssuers:        make(map[string]int),
		IssuerCNFilter:      issuerCNList,
		EarliestDateFilter:  earliestDate,
		CorrelateLogEntries: *config.CorrelateLogEntries,
		LogExpiredEntries:   *config.LogExpiredEntries,
	}
	err = entriesDb.InitTables()
	if err != nil {
		log.Fatalf("unable to prepare SQL: %s: %s", dbConnectStr, err)
	}

	logUrls := []url.URL{}

	if config.LogUrl != nil && len(*config.LogUrl) > 5 {
		ctLogUrl, err := url.Parse(*config.LogUrl)
		if err != nil {
			log.Fatalf("unable to set Certificate Log: %s", err)
		}
		logUrls = append(logUrls, *ctLogUrl)
	}

	if config.LogUrlList != nil && len(*config.LogUrlList) > 5 {
		for _, part := range strings.Split(*config.LogUrlList, ",") {
			ctLogUrl, err := url.Parse(strings.TrimSpace(part))
			if err != nil {
				log.Fatalf("unable to set Certificate Log: %s", err)
			}
			logUrls = append(logUrls, *ctLogUrl)
		}
	}

	if len(logUrls) > 0 {
		logDownloader := NewLogDownloader(entriesDb)
		logDownloader.Display.StartDisplay(logDownloader.ThreadWaitGroup)
		logDownloader.StartThreads()

		for _, ctLogUrl := range logUrls {
			urlString := ctLogUrl.String()
			log.Printf("[%s] Starting download. FullCerts=%t\n", urlString, (certFolderDB != nil))

			logDownloader.DownloaderWaitGroup.Add(1)
			go func() {
				defer logDownloader.DownloaderWaitGroup.Done()

				sigChan := make(chan os.Signal, 1)
				signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
				defer signal.Stop(sigChan)
				defer close(sigChan)

				for {
					logDownloader.Download(urlString)
					if !*config.RunForever {
						return
					}
					sleepTime := time.Duration(*config.PollingDelay) * time.Minute
					log.Printf("[%s] Completed. Polling again in %s.\n", urlString, sleepTime)

					select {
					case <-sigChan:
						log.Printf("[%s] Signal caught.\n", urlString)
						return
					case <-time.After(sleepTime):
						continue
					}
				}
			}()
		}

		logDownloader.DownloaderWaitGroup.Wait() // Wait for downloaders to stop
		logDownloader.Stop()                     // Stop workers
		logDownloader.ThreadWaitGroup.Wait()     // Wait for workers to stop
		os.Exit(0)
	}

	var importer censysdata.Importer
	if config.CensysUrl != nil && len(*config.CensysUrl) > 5 {
		urlImporter, err := censysdata.OpenURL(*config.CensysUrl)
		if err != nil {
			log.Fatalf("unable to open Censys URL: %s", err)
		}
		importer = urlImporter
	} else if config.CensysPath != nil && len(*config.CensysPath) > 5 {
		fileImporter, err := censysdata.OpenFile(*config.CensysPath)
		if err != nil {
			log.Fatalf("unable to open Censys file: %s", err)
		}
		importer = fileImporter
		defer fileImporter.Close()
	} else if *config.CensysStdin {
		stdinImporter, err := censysdata.OpenFileHandle(os.Stdin)
		if err != nil {
			log.Fatalf("unable to open stdin: %s", err)
		}
		importer = stdinImporter
		defer stdinImporter.Close()
	}

	if importer != nil {
		log.Printf("Starting Censys Import, using %s, fullCerts=%t\n", importer.String(), (certFolderDB != nil))

		wg := new(sync.WaitGroup)
		err = processImporter(importer, entriesDb, wg)

		if err != nil {
			log.Fatalf("error while running importer: %s", err)
		}

		wg.Wait()
		os.Exit(0)
	}

	// Didn't include a mandatory action, so print usage and exit.
	config.Usage()
	os.Exit(2)
}
