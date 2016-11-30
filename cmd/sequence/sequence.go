// Copyright (c) 2014 Dataence, LLC. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Sequence is a high performance sequential log scanner, analyzer and parser.
// It sequentially goes through a log message, parses out the meaningful parts,
// without the use regular expressions. It can parse over 100,000 messages per
// second without the need to separate parsing rules by log source type.
//
// Documentation and other information are available at sequencer.io
package main

import (
	"bufio"
	"compress/gzip"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/surge/glog"
	"github.com/trustpath/sequence"
)

var (
	cfgfile    string
	infile     string
	outfile    string
	patfile    string
	cpuprofile string
	workers    int
	format     string

	quit chan struct{}
	done chan struct{}
)

const (
	mbyte = 1024 * 1024
)

type dataSlice []sortableStruct

type pMapStruct struct {
	ex  string
	cnt int
}

type sortableStruct struct {
	ex  string
	cnt int
	pat string
}

// Len is part of sort.Interface.
func (d dataSlice) Len() int {
	return len(d)
}

// Swap is part of sort.Interface.
func (d dataSlice) Swap(i, j int) {
	d[i], d[j] = d[j], d[i]
}

// Less is part of sort.Interface. We use count as the value to sort by
func (d dataSlice) Less(i, j int) bool {
	return d[i].cnt > d[j].cnt
}

func profile() {
	var f *os.File
	var err error

	if cpuprofile != "" {
		f, err = os.Create(cpuprofile)
		if err != nil {
			log.Fatal(err)
		}

		pprof.StartCPUProfile(f)
	}

	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, os.Interrupt, os.Kill)
	go func() {
		select {
		case sig := <-sigchan:
			log.Printf("Existing due to trapped signal; %v", sig)

		case <-quit:
			log.Println("Quiting...")

		}

		if f != nil {
			glog.Errorf("Stopping profile")
			pprof.StopCPUProfile()
			f.Close()
		}

		close(done)
		os.Exit(0)
	}()
}

func scan(cmd *cobra.Command, args []string) {
	readConfig()

	scanner := sequence.NewScanner()

	if infile != "" {
		// Open input file
		iscan, ifile := openInputFile(infile)
		defer ifile.Close()

		ofile := openOutputFile(outfile)
		defer ofile.Close()

		for iscan.Scan() {
			line := iscan.Text()
			if len(line) == 0 || line[0] == '#' {
				continue
			}

			seq := scanMessage(scanner, line)
			fmt.Fprintf(ofile, "%s\n\n", seq.PrintTokens())
		}
	} else if len(args) == 1 && args[0] != "" {
		seq := scanMessage(scanner, args[0])
		fmt.Println(seq.PrintTokens())
	} else {
		log.Fatal("Invalid input file or string specified")
	}
}

func analyze(cmd *cobra.Command, args []string) {
	readConfig()

	if infile == "" {
		log.Fatal("Invalid input file specified")
	}

	profile()

	parser := buildParser()
	analyzer := sequence.NewAnalyzer()
	scanner := sequence.NewScanner()

	// Open input file
	iscan, ifile := openInputFile(infile)
	defer ifile.Close()

	// For all the log messages, if we can't parse it, then let's add it to the
	// analyzer for pattern analysis
	for iscan.Scan() {
		line := iscan.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		seq := scanMessage(scanner, line)

		if _, err := parser.Parse(seq); err != nil {
			analyzer.Add(seq)
		}
	}

	ifile.Close()
	analyzer.Finalize()

	iscan, ifile = openInputFile(infile)
	defer ifile.Close()

	pmap := make(map[string]pMapStruct)
	amap := make(map[string]pMapStruct)
	n := 0

	// Now that we have built the analyzer, let's go through each log message again
	// to determine the unique patterns
	for iscan.Scan() {
		line := iscan.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		n++

		seq := scanMessage(scanner, line)

		pseq, err := parser.Parse(seq)
		if err == nil {
			pat := pseq.String()
			stat, ok := pmap[pat]
			if !ok {
				stat = struct {
					ex  string
					cnt int
				}{}
			}
			stat.ex = line
			stat.cnt++
			pmap[pat] = stat
		} else {
			aseq, err := analyzer.Analyze(seq)
			if err != nil {
				log.Printf("Error analyzing: %s", line)
			} else {
				pat := aseq.String()
				stat, ok := amap[pat]
				if !ok {
					stat = struct {
						ex  string
						cnt int
					}{}
				}
				stat.ex = line
				stat.cnt++
				amap[pat] = stat
			}
		}
	}

	ofile := openOutputFile(outfile)
	defer ofile.Close()

	// for pat, stat := range pmap {
	// 	fmt.Fprintf(ofile, "%s\n# %d log messages matched\n# %s\n\n", pat, stat.cnt, stat.ex)
	// }

	// for pat, stat := range amap {
	// 	fmt.Fprintf(ofile, "%s\n# %d log messages matched\n# %s\n\n", pat, stat.cnt, stat.ex)
	// }

	s := make(dataSlice, 0, len(amap))

	for pat, d := range amap {
		s = append(s, sortableStruct{ex: d.ex, cnt: d.cnt, pat: pat})
	}
	sort.Sort(s)
	for _, stat := range s {
		fmt.Fprintf(ofile, "# %d log messages matched\n%v\n# %s\n\n", stat.cnt, stat.pat, stat.ex)
	}

	log.Printf("Analyzed %d messages, found %d unique patterns, %d are new.", n, len(pmap)+len(amap), len(amap))
}

func parse(cmd *cobra.Command, args []string) {
	readConfig()

	if infile == "" {
		log.Fatal("Invalid input file specified")
	}

	profile()

	parser := buildParser()
	scanner := sequence.NewScanner()

	iscan, ifile := openInputFile(infile)
	defer ifile.Close()

	ofile := openOutputFile(outfile)
	defer ofile.Close()

	n := 0
	now := time.Now()

	for iscan.Scan() {
		line := iscan.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		n++

		seq := scanMessage(scanner, line)

		seq, err := parser.Parse(seq)
		if err != nil {
			log.Printf("Error (%s) parsing: %s", err, line)
		} else {
			fmt.Fprintf(ofile, "%s\n%s\n\n", line, seq.PrintTokens())
		}
	}

	since := time.Since(now)
	log.Printf("Parsed %d messages in %.2f secs, ~ %.2f msgs/sec", n, float64(since)/float64(time.Second), float64(n)/(float64(since)/float64(time.Second)))
	close(quit)
	<-done
}

func benchScan(cmd *cobra.Command, args []string) {
	readConfig()

	iscan, ifile := openInputFile(infile)
	defer ifile.Close()

	var lines []string
	var totalSize int
	n := 0

	for iscan.Scan() {
		line := iscan.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		n++
		lines = append(lines, line)
		totalSize += len(line)
	}

	profile()

	now := time.Now()

	if workers == 1 {
		scanner := sequence.NewScanner()
		for _, line := range lines {
			scanMessage(scanner, line)
		}
	} else {
		var wg sync.WaitGroup
		msgpipe := make(chan string, 10000)

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				scanner := sequence.NewScanner()

				for line := range msgpipe {
					scanMessage(scanner, line)
				}
			}()
		}

		for _, line := range lines {
			msgpipe <- line
		}
		close(msgpipe)

		wg.Wait()
	}

	since := time.Since(now)
	log.Printf("Scanned %d messages in %.2f secs, ~ %.2f msgs/sec, ~ %.2f MB/sec", n, float64(since)/float64(time.Second), float64(n)/(float64(since)/float64(time.Second)), float64(totalSize)/float64(mbyte)/(float64(since)/float64(time.Second)))
	close(quit)
	<-done
}

func benchParse(cmd *cobra.Command, args []string) {
	readConfig()

	if infile == "" {
		log.Fatal("Invalid input file")
	}

	parser := buildParser()

	iscan, ifile := openInputFile(infile)
	defer ifile.Close()

	var lines []string
	var totalSize int
	n := 0

	for iscan.Scan() {
		line := iscan.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}

		n++
		lines = append(lines, line)
		totalSize += len(line)
	}

	profile()

	now := time.Now()

	if workers == 1 {
		scanner := sequence.NewScanner()

		for _, line := range lines {
			parser.Parse(scanMessage(scanner, line))
		}
	} else {
		var wg sync.WaitGroup
		msgpipe := make(chan string, 10000)

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				scanner := sequence.NewScanner()

				for line := range msgpipe {
					parser.Parse(scanMessage(scanner, line))
				}
			}()
		}

		for _, line := range lines {
			msgpipe <- line
		}
		close(msgpipe)

		wg.Wait()
	}

	since := time.Since(now)
	log.Printf("Parsed %d messages in %.2f secs, ~ %.2f msgs/sec, ~ %.2f MB/sec", n, float64(since)/float64(time.Second), float64(n)/(float64(since)/float64(time.Second)), float64(totalSize)/float64(mbyte)/(float64(since)/float64(time.Second)))
	close(quit)
	<-done
}

func scanMessage(scanner *sequence.Scanner, data string) sequence.Sequence {
	var (
		seq sequence.Sequence
		err error
	)

	switch format {
	case "json":
		seq, err = scanner.ScanJson(data)

	default:
		seq, err = scanner.Scan(data)
	}

	if err != nil {
		log.Fatal(err)
	}
	return seq
}

func buildParser() *sequence.Parser {
	parser := sequence.NewParser()

	if patfile == "" {
		return parser
	}

	var files []string

	if fi, err := os.Stat(patfile); err != nil {
		log.Fatal(err)
	} else if fi.Mode().IsDir() {
		files = getDirOfFiles(patfile)
	} else {
		files = append(files, patfile)
	}

	scanner := sequence.NewScanner()

	for _, file := range files {
		// Open pattern file
		pscan, pfile := openInputFile(file)

		for pscan.Scan() {
			line := pscan.Text()
			if len(line) == 0 || line[0] == '#' {
				continue
			}

			seq, err := scanner.Scan(line)
			if err != nil {
				log.Fatal(err)
			}

			if err := parser.Add(seq); err != nil {
				log.Fatal(err)
			}
		}

		pfile.Close()
	}

	return parser
}

func openInputFile(fname string) (*bufio.Scanner, *os.File) {
	var s *bufio.Scanner

	f, err := os.Open(fname)
	if err != nil {
		log.Fatal(err)
	}

	if strings.HasSuffix(fname, ".gz") {
		gunzip, err := gzip.NewReader(f)
		if err != nil {
			log.Fatal(err)
		}

		s = bufio.NewScanner(gunzip)
	} else {
		s = bufio.NewScanner(f)
	}

	return s, f
}

func getDirOfFiles(path string) []string {
	filenames := make([]string, 0, 10)

	files, err := ioutil.ReadDir(path)
	if err != nil {
		log.Fatal(err)
	}

	for _, f := range files {
		filenames = append(filenames, path+"/"+f.Name())
	}

	return filenames
}

func openOutputFile(fname string) *os.File {
	var (
		ofile *os.File
		err   error
	)

	if fname == "" {
		ofile = os.Stdin
	} else {
		// Open output file
		ofile, err = os.OpenFile(fname, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			log.Fatal(err)
		}
	}

	return ofile
}

func readConfig() {
	if cfgfile == "" {
		cfgfile = "./sequence.toml"

		if _, err := os.Stat(cfgfile); err != nil {
			if slash := strings.LastIndex(os.Args[0], "/"); slash != -1 {
				cfgfile = os.Args[0][:slash] + "/sequence.toml"

				if _, err := os.Stat(cfgfile); err != nil {
					log.Fatalln("No configuration file found")
				}
			}
		}
	}

	if err := sequence.ReadConfig(cfgfile); err != nil {
		log.Fatal(err)
	}
}

func main() {
	quit = make(chan struct{})
	done = make(chan struct{})

	var (
		sequenceCmd = &cobra.Command{
			Use:   "sequence",
			Short: "sequence is a high performance sequential log analyzer and parser",
		}

		scanCmd = &cobra.Command{
			Use:   "scan",
			Short: "tokenizes a log file or message and output a list of tokens",
		}

		analyzeCmd = &cobra.Command{
			Use:   "analyze",
			Short: "analyzes a log file and output a list of patterns that will match all the log messages",
		}

		parseCmd = &cobra.Command{
			Use:   "parse",
			Short: "parses a log file and output a list of parsed tokens for each of the log messages",
		}

		benchCmd = &cobra.Command{
			Use:   "bench",
			Short: "benchmarks scanning or parsing of a log file, no output is provided",
		}

		benchScanCmd = &cobra.Command{
			Use:   "scan",
			Short: "benchmarks the scanning of a log file, no output is provided",
		}

		benchParseCmd = &cobra.Command{
			Use:   "parse",
			Short: "benchmarks the parsing of a log file, no output is provided",
		}
	)

	sequenceCmd.PersistentFlags().StringVarP(&cfgfile, "config", "", "", "TOML-formatted configuration file, default checks ./sequence.toml, then sequence.toml in the same directory as program")
	sequenceCmd.PersistentFlags().StringVarP(&format, "format", "", "", "format of the message to tokenize, can be 'json' or leave empty")
	sequenceCmd.PersistentFlags().StringVarP(&infile, "input", "i", "", "input file, required")
	sequenceCmd.PersistentFlags().StringVarP(&outfile, "output", "o", "", "output file, if empty, to stdout")
	sequenceCmd.PersistentFlags().StringVarP(&patfile, "patterns", "p", "", "patterns, can be a file or directory, used by analyze and parse")

	benchCmd.PersistentFlags().StringVarP(&cpuprofile, "cpuprofile", "", "", "CPU profile filename")
	benchCmd.PersistentFlags().IntVarP(&workers, "workers", "", 1, "number of parsing workers")

	scanCmd.Run = scan
	analyzeCmd.Run = analyze
	parseCmd.Run = parse
	benchScanCmd.Run = benchScan
	benchParseCmd.Run = benchParse

	benchCmd.AddCommand(benchScanCmd)
	benchCmd.AddCommand(benchParseCmd)

	sequenceCmd.AddCommand(scanCmd)
	sequenceCmd.AddCommand(analyzeCmd)
	sequenceCmd.AddCommand(parseCmd)
	sequenceCmd.AddCommand(benchCmd)

	sequenceCmd.Execute()
}
