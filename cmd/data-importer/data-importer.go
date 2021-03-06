package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/adjust/rmq/v4"
	"github.com/britbus/britbus/pkg/bods"
	"github.com/britbus/britbus/pkg/ctdf"
	"github.com/britbus/britbus/pkg/database"
	"github.com/britbus/britbus/pkg/elastic_client"
	"github.com/britbus/britbus/pkg/naptan"
	"github.com/britbus/britbus/pkg/redis_client"
	"github.com/britbus/britbus/pkg/siri_vm"
	"github.com/britbus/britbus/pkg/transforms"
	"github.com/britbus/britbus/pkg/transxchange"
	travelinenoc "github.com/britbus/britbus/pkg/traveline_noc"
	"github.com/britbus/britbus/pkg/util"
	"github.com/britbus/notify/pkg/notify_client"
	"github.com/urfave/cli/v2"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	_ "time/tzdata"
)

var realtimeQueue rmq.Queue

type DataFile struct {
	Name      string
	Reader    io.Reader
	Overrides map[string]string
}

func tempDownloadFile(source string) (*os.File, string) {
	resp, err := http.Get(source)

	if err != nil {
		log.Fatal().Err(err).Msg("Download file")
	}
	defer resp.Body.Close()

	_, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	fileExtension := filepath.Ext(source)
	if err == nil {
		fileExtension = filepath.Ext(params["filename"])
	}

	tmpFile, err := ioutil.TempFile(os.TempDir(), "britbus-data-importer-")
	if err != nil {
		log.Fatal().Err(err).Msg("Cannot create temporary file")
	}

	io.Copy(tmpFile, resp.Body)

	return tmpFile, fileExtension
}

func importFile(dataFormat string, source string, fileFormat string, overrides map[string]string) error {
	dataFiles := []DataFile{}
	fileExtension := filepath.Ext(source)

	// Check if the source is a URL and load the http client stream if it is
	if isValidUrl(source) {
		var tempFile *os.File
		tempFile, fileExtension = tempDownloadFile(source)

		source = tempFile.Name()
		defer os.Remove(tempFile.Name())
	}

	if fileFormat != "" {
		fileExtension = fmt.Sprintf(".%s", fileFormat)
	}

	// Check if its an XML file or ZIP file

	if fileExtension == ".xml" {
		file, err := os.Open(source)
		if err != nil {
			log.Fatal().Err(err).Msg("Failed to open file")
		}
		defer file.Close()

		dataFiles = append(dataFiles, DataFile{
			Name:      source,
			Reader:    file,
			Overrides: overrides,
		})
	} else if fileExtension == ".zip" {
		archive, err := zip.OpenReader(source)
		if err != nil {
			panic(err)
		}
		defer archive.Close()

		for _, zipFile := range archive.File {
			file, err := zipFile.Open()
			if err != nil {
				log.Fatal().Err(err).Msg("Failed to open file")
			}
			defer file.Close()

			dataFiles = append(dataFiles, DataFile{
				Name:      fmt.Sprintf("%s:%s", source, zipFile.Name),
				Reader:    file,
				Overrides: overrides,
			})
		}
	} else {
		return errors.New(fmt.Sprintf("Unsupported file extension %s", fileExtension))
	}

	for _, dataFile := range dataFiles {
		err := parseDataFile(dataFormat, &dataFile)

		if err != nil {
			return err
		}
	}

	return nil
}

func parseDataFile(dataFormat string, dataFile *DataFile) error {
	switch dataFormat {
	case "naptan":
		log.Info().Msgf("NaPTAN file import from %s", dataFile.Name)
		naptanDoc, err := naptan.ParseXMLFile(dataFile.Reader, naptan.BusFilter)

		if err != nil {
			return err
		}

		naptanDoc.ImportIntoMongoAsCTDF(&ctdf.DataSource{
			Provider: "Department of Transport",
			Dataset:  dataFile.Name,
		})
	case "traveline-noc":
		log.Info().Msgf("Traveline NOC file import from %s", dataFile.Name)
		travelineData, err := travelinenoc.ParseXMLFile(dataFile.Reader)

		if err != nil {
			return err
		}

		travelineData.ImportIntoMongoAsCTDF(&ctdf.DataSource{
			Dataset: dataFile.Name,
		})
	case "transxchange":
		log.Info().Msgf("TransXChange file import from %s ", dataFile.Name)
		transXChangeDoc, err := transxchange.ParseXMLFile(dataFile.Reader)

		if err != nil {
			return err
		}

		transXChangeDoc.ImportIntoMongoAsCTDF(&ctdf.DataSource{
			Provider: "Department of Transport", // This may not always be true
			Dataset:  dataFile.Name,
		}, dataFile.Overrides)
	case "siri-vm":
		log.Info().Msgf("Siri-VM file import from %s ", dataFile.Name)

		err := siri_vm.ParseXMLFile(dataFile.Reader, realtimeQueue, &ctdf.DataSource{
			Provider: "Department of Transport", // This may not always be true
			Dataset:  dataFile.Name,
		})

		if err != nil {
			return err
		}
	default:
		return errors.New(fmt.Sprintf("Unsupported data-format %s", dataFormat))
	}

	return nil
}

func main() {
	// Overwrite internal timezone location to UK time
	loc, _ := time.LoadLocation("Europe/London")
	time.Local = loc

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339})

	transforms.SetupClient()

	if err := database.Connect(); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to database")
	}
	if err := elastic_client.Connect(false); err != nil {
		log.Fatal().Err(err).Msg("Failed to connect to Elasticsearch")
	}

	// Setup the notifications client
	notify_client.Setup()

	ctdf.LoadSpecialDayCache()

	app := &cli.App{
		Name:        "data-importer",
		Description: "Manages ingesting and verifying data in BritBus",
		Commands: []*cli.Command{
			{
				Name:  "file",
				Usage: "Import a dataset into BritBus",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "repeat-every",
						Usage:    "Repeat this file import every X seconds",
						Required: false,
					},
					&cli.StringFlag{
						Name:     "file-format",
						Usage:    "Overwrite the file format (eg. zip or xml)",
						Required: false,
					},
				},
				ArgsUsage: "<data-format> <source>",
				Action: func(c *cli.Context) error {
					if c.Args().Len() != 2 {
						return errors.New("<data-format> and <source> must be provided")
					}

					fileFormat := c.String("file-format")

					repeatEvery := c.String("repeat-every")
					repeat := repeatEvery != ""
					var repeatDuration time.Duration
					if repeat {
						var err error
						repeatDuration, err = time.ParseDuration(repeatEvery)

						if err != nil {
							return err
						}
					}

					dataFormat := c.Args().Get(0)
					source := c.Args().Get(1)

					// Some initial setup for Siri-VM
					if dataFormat == "siri-vm" {
						if err := redis_client.Connect(); err != nil {
							log.Fatal().Err(err).Msg("Failed to connect to Redis")
						}

						var err error
						realtimeQueue, err = redis_client.QueueConnection.OpenQueue("realtime-queue")
						if err != nil {
							log.Fatal().Err(err).Msg("Failed to start siri-vm redis queue")
						}

						//TODO: TEMPORARY
						// Get the API key from the environment variables and append to the source URL
						env := util.GetEnvironmentVariables()
						if env["BRITBUS_BODS_API_KEY"] != "" {
							source += fmt.Sprintf("?api_key=%s", env["BRITBUS_BODS_API_KEY"])
						}
					}

					for {
						startTime := time.Now()

						err := importFile(dataFormat, source, fileFormat, map[string]string{})

						if err != nil {
							return err
						}

						if !repeat {
							break
						}

						executionDuration := time.Since(startTime)
						log.Info().Msgf("Operation took %s", executionDuration.String())

						waitTime := repeatDuration - executionDuration

						if waitTime.Seconds() > 0 {
							time.Sleep(waitTime)
						}
					}

					return nil
				},
			},
			{
				Name:  "bods-timetable",
				Usage: "Import TransXChange Timetable datasets from BODS into BritBus",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "url",
						Usage:    "Overwrite URL for the BODS Timetable API",
						Required: false,
					},
				},
				Action: func(c *cli.Context) error {
					source := c.String("url")

					// Default source of all published buses
					if source == "" {
						source = "https://data.bus-data.dft.gov.uk/api/v1/dataset/?limit=25&offset=0&status=published"
					}

					log.Info().Msgf("Bus Open Data Service Timetable API import from %s ", source)

					// Get the API key from the environment variables and append to the source URL
					env := util.GetEnvironmentVariables()
					if env["BRITBUS_BODS_API_KEY"] != "" {
						source += fmt.Sprintf("&api_key=%s", env["BRITBUS_BODS_API_KEY"])
					}

					timeTableDataset, err := bods.GetTimetableDataset(source)
					log.Info().Msgf(" - %d datasets", len(timeTableDataset))

					if err != nil {
						return err
					}

					datasetVersionsCollection := database.GetCollection("dataset_versions")

					for _, dataset := range timeTableDataset {
						var datasetVersion *ctdf.DatasetVersion

						query := bson.M{"$and": bson.A{
							bson.M{"dataset": "GB-BODS"},
							bson.M{"identifier": fmt.Sprintf("%d", dataset.ID)},
						}}
						datasetVersionsCollection.FindOne(context.Background(), query).Decode(&datasetVersion)

						if datasetVersion == nil || datasetVersion.LastModified != dataset.Modified {
							err = importFile("transxchange", dataset.URL, "", map[string]string{})

							if err != nil {
								log.Error().Err(err).Msgf("Failed to import file %s (%s)", dataset.Name, dataset.URL)
								continue
							}

							if datasetVersion == nil {
								datasetVersion = &ctdf.DatasetVersion{
									Dataset:    "GB-BODS",
									Identifier: fmt.Sprintf("%d", dataset.ID),
								}
							}
							datasetVersion.LastModified = dataset.Modified

							opts := options.Update().SetUpsert(true)
							datasetVersionsCollection.UpdateOne(context.Background(), query, bson.M{"$set": datasetVersion}, opts)
						} else {
							log.Info().Int("id", dataset.ID).Msg("Dataset not changed")
						}
					}

					return nil
				},
			},
			{
				Name:  "tfl",
				Usage: "Import Transport for London (TfL) datasets from their API into BritBus",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:     "url",
						Usage:    "Overwrite URL for the TfL TransXChange file",
						Required: false,
					},
				},
				Action: func(c *cli.Context) error {
					source := c.String("url")

					// Default source of all published buses
					if source == "" {
						source = "https://tfl.gov.uk/tfl/syndication/feeds/journey-planner-timetables.zip"
					}

					log.Info().Msgf("TfL TransXChange import from %s", source)

					if isValidUrl(source) {
						tempFile, _ := tempDownloadFile(source)

						source = tempFile.Name()
						defer os.Remove(tempFile.Name())
					}

					archive, err := zip.OpenReader(source)
					if err != nil {
						panic(err)
					}
					defer archive.Close()

					tflBusesMatchRegex, _ := regexp.Compile("(?i)BUSES PART \\w+ \\d+.zip")
					for _, zipFile := range archive.File {
						if tflBusesMatchRegex.MatchString(zipFile.Name) {
							file, err := zipFile.Open()
							if err != nil {
								log.Fatal().Err(err).Msg("Failed to open file")
							}
							defer file.Close()

							tmpFile, err := ioutil.TempFile(os.TempDir(), "britbus-data-importer-tfl-innerzip-")
							if err != nil {
								log.Fatal().Err(err).Msg("Cannot create temporary file")
							}
							defer os.Remove(tmpFile.Name())

							io.Copy(tmpFile, file)

							err = importFile("transxchange", tmpFile.Name(), "zip", map[string]string{
								"OperatorRef": "GB:NOC:TFLO",
							})
							if err != nil {
								log.Fatal().Err(err).Msg("Cannot import TfL inner zip")
							}
						}
					}

					return nil
				},
			},
		},
	}

	err := app.Run(os.Args)

	notify_client.Await()

	if err != nil {
		log.Fatal().Err(err).Send()
	}
}

func isValidUrl(toTest string) bool {
	_, err := url.ParseRequestURI(toTest)
	if err != nil {
		return false
	}

	u, err := url.Parse(toTest)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}

	return true
}
