package main

import (
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/fatih/structs"
	"github.com/gorilla/mux"
	"github.com/malice-plugins/go-plugin-utils/database/elasticsearch"
	"github.com/malice-plugins/go-plugin-utils/utils"
	"github.com/parnurzeal/gorequest"
	"github.com/urfave/cli"
	"github.com/willf/bloom"
)

var (
	// Version stores the plugin's version
	Version string

	// BuildTime stores the plugin's build time
	BuildTime string

	// ErrorRate stores the bloomfilter desired error-rate
	ErrorRate string

	// HashType is the type of hash to use to build the bloomfilter
	HashType string
)

const (
	// NSRL fields
	sha1         = 0
	md5          = 1
	crc32        = 2
	fileName     = 3
	fileSize     = 4
	productCode  = 5
	opSystemCode = 6
	specialCode  = 7
)

const (
	name     = "nsrl"
	category = "intel"
)

type pluginResults struct {
	ID   string      `json:"id" gorethink:"id,omitempty"`
	Data ResultsData `json:"nsrl" gorethink:"nsrl"`
}

// Nsrl json object
type Nsrl struct {
	Results ResultsData `json:"nsrl"`
}

// ResultsData json object
type ResultsData struct {
	Found    bool   `json:"found"`
	MarkDown string `json:"markdown,omitempty" structs:"markdown,omitempty"`
}

func generateMarkDownTable(b Bitdefender) string {
	var tplOut bytes.Buffer

	t := template.Must(template.New("bit").Parse(tpl))

	err := t.Execute(&tplOut, b)
	if err != nil {
		log.Println("executing template:", err)
	}

	return tplOut.String()
}

func lineCounter(r io.Reader) (uint64, error) {
	buf := make([]byte, 32*1024)
	var count uint64
	lineSep := []byte{'\n'}

	for {
		c, err := r.Read(buf)
		count += uint64(bytes.Count(buf[:c], lineSep))

		switch {
		case err == io.EOF:
			return count, nil

		case err != nil:
			return count, err
		}
	}
}

// build bloomfilter from NSRL database
func buildFilter() {

	// open NSRL database
	nsrlDB, err := os.Open("NSRLFile.txt")
	utils.Assert(err)
	defer nsrlDB.Close()
	// count lines in NSRL database
	lines, err := lineCounter(nsrlDB)
	log.Debugf("Number of lines in NSRLFile.txt: %d", lines)
	// write line count to file LINECOUNT
	buf := new(bytes.Buffer)
	utils.Assert(binary.Write(buf, binary.LittleEndian, lines))
	utils.Assert(ioutil.WriteFile("LINECOUNT", buf.Bytes(), 0644))

	// Create new bloomfilter with size = number of lines in NSRL database
	erate, err := strconv.ParseFloat(ErrorRate, 64)
	utils.Assert(err)

	filter := bloom.NewWithEstimates(uint(lines), erate)

	// jump back to the begining of the file
	_, err = nsrlDB.Seek(0, io.SeekStart)
	utils.Assert(err)

	log.Debug("Loading NSRL database into bloomfilter")
	reader := csv.NewReader(nsrlDB)
	// strip off csv header
	_, _ = reader.Read()
	for {
		record, err := reader.Read()

		if err == io.EOF {
			break
		}
		utils.Assert(err)

		// log.Debug(record)
		filter.Add([]byte(record[md5]))
	}

	bloomFile, err := os.Create("nsrl.bloom")
	utils.Assert(err)
	defer bloomFile.Close()

	log.Debug("Writing bloomfilter to disk")
	_, err = filter.WriteTo(bloomFile)
	utils.Assert(err)
}

// lookUp queries the NSRL bloomfilter for a hash
func lookUp(hash string, timeout int) ResultsData {

	var lines uint64
	nsrlResults := ResultsData{}

	// read line count from file LINECOUNT
	lineCount, err := ioutil.ReadFile("LINECOUNT")
	utils.Assert(err)
	buf := bytes.NewReader(lineCount)
	utils.Assert(binary.Read(buf, binary.LittleEndian, &lines))
	log.Debugf("Number of lines in NSRLFile.txt: %d", lines)

	// Create new bloomfilter with size = number of lines in NSRL database
	erate, err := strconv.ParseFloat(ErrorRate, 64)
	filter := bloom.NewWithEstimates(uint(lines), erate)

	// load NSRL bloomfilter from file
	f, err := os.Open("nsrl.bloom")
	utils.Assert(err)
	_, err = filter.ReadFrom(f)
	utils.Assert(err)

	// test of existance of hash in bloomfilter
	nsrlResults.Found = filter.TestString(hash)

	return nsrlResults
}

func webService() {
	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/lookup/{md5}", webLookUp)
	log.Info("web service listening on port :3993")
	log.Fatal(http.ListenAndServe(":3993", router))
}

func webLookUp(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	hash := vars["md5"]

	hashType, _ := utils.GetHashType(hash)

	if strings.EqualFold(hashType, "md5") {
		nsrl := Nsrl{Results: lookUp(strings.ToUpper(hash), 10)}

		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		if nsrl.Results.Found {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}

		if err := json.NewEncoder(w).Encode(nsrl); err != nil {
			panic(err)
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "Please supply a proper MD5 hash to query")
	}
}

func printStatus(resp gorequest.Response, body string, errs []error) {
	fmt.Println(body)
}

func main() {

	var elastic string

	cli.AppHelpTemplate = utils.AppHelpTemplate
	app := cli.NewApp()

	app.Name = "nsrl"
	app.Author = "blacktop"
	app.Email = "https://github.com/blacktop"
	app.Version = Version + ", BuildTime: " + BuildTime
	app.Compiled, _ = time.Parse("20060102", BuildTime)
	app.Usage = "Malice NSRL Plugin"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "verbose, V",
			Usage: "verbose output",
		},
		cli.StringFlag{
			Name:        "elasitcsearch",
			Value:       "",
			Usage:       "elasitcsearch address for Malice to store results",
			EnvVar:      "MALICE_ELASTICSEARCH",
			Destination: &elastic,
		},
		cli.BoolFlag{
			Name:   "post, p",
			Usage:  "POST results to Malice webhook",
			EnvVar: "MALICE_ENDPOINT",
		},
		cli.BoolFlag{
			Name:   "proxy, x",
			Usage:  "proxy settings for Malice webhook endpoint",
			EnvVar: "MALICE_PROXY",
		},
		cli.BoolFlag{
			Name:  "table, t",
			Usage: "output as Markdown table",
		},
		cli.IntFlag{
			Name:   "timeout",
			Value:  10,
			Usage:  "malice plugin timeout (in seconds)",
			EnvVar: "MALICE_TIMEOUT",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:    "web",
			Aliases: []string{"w"},
			Usage:   "Create a NSRL lookup web service",
			Action: func(c *cli.Context) error {
				webService()
				return nil
			},
		},
		{
			Name:    "build",
			Aliases: []string{"b"},
			Usage:   "Build bloomfilter from NSRL database",
			Action: func(c *cli.Context) error {
				if c.GlobalBool("verbose") {
					log.SetLevel(log.DebugLevel)
				}

				buildFilter()
				return nil
			},
		},
		{
			Name:      "lookup",
			Aliases:   []string{"l"},
			Usage:     "Query NSRL for hash",
			ArgsUsage: "MD5 to query NSRL with",
			Action: func(c *cli.Context) error {
				if c.Args().Present() {
					hash := strings.ToUpper(c.Args().First())
					hashType, _ := utils.GetHashType(hash)

					if !strings.EqualFold(hashType, "md5") {
						log.Fatal(fmt.Errorf("Please supply a valid MD5 hash to query NSRL with."))
					}

					if c.GlobalBool("verbose") {
						log.SetLevel(log.DebugLevel)
					}

					nsrl := Nsrl{Results: lookUp(hash, c.GlobalInt("timeout"))}
					nsrl.Results.MarkDown =
						// upsert into Database
						elasticsearch.InitElasticSearch(elastic)
					elasticsearch.WritePluginResultsToDatabase(elasticsearch.PluginResults{
						ID:       utils.Getopt("MALICE_SCANID", hash),
						Name:     name,
						Category: category,
						Data:     structs.Map(nsrl.Results),
					})

					if c.GlobalBool("table") {
						printMarkDownTable(nsrl)
					} else {
						nsrlJSON, err := json.Marshal(nsrl)
						utils.Assert(err)
						if c.GlobalBool("post") {
							request := gorequest.New()
							if c.GlobalBool("proxy") {
								request = gorequest.New().Proxy(os.Getenv("MALICE_PROXY"))
							}
							request.Post(os.Getenv("MALICE_ENDPOINT")).
								Set("X-Malice-ID", utils.Getopt("MALICE_SCANID", hash)).
								Send(string(nsrlJSON)).
								End(printStatus)

							return nil
						}
						fmt.Println(string(nsrlJSON))
					}
				} else {
					log.Fatal(fmt.Errorf("Please supply a MD5 hash to query NSRL with."))
				}
				return nil
			},
		},
	}

	err := app.Run(os.Args)
	utils.Assert(err)
}
