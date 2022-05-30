package archiver

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"github.com/britbus/britbus/pkg/ctdf"
	"github.com/britbus/britbus/pkg/database"
	"github.com/rs/zerolog/log"
	"go.mongodb.org/mongo-driver/bson"
)

type Archiver struct {
	OutputDirectory     string
	WriteIndividualFile bool
	WriteBundle         bool
	CloudUpload         bool
	CloudBucketName     string
}

func (a *Archiver) Perform() {
	log.Info().Str("outputDirectory", a.OutputDirectory).Bool("individualFile", a.WriteIndividualFile).Bool("bundle", a.WriteBundle).Msg("Running Archive process")

	currentTime := time.Now()
	cutOffTime := currentTime.Add(-24 * time.Hour)
	log.Info().Msgf("Archiving realtime journeys older than %s", cutOffTime)

	realtimeJourneysCollection := database.GetCollection("realtime_journeys")
	cursor, _ := realtimeJourneysCollection.Find(context.Background(), bson.M{"modificationdatetime": bson.M{"$lt": cutOffTime}})

	recordCount := 0

	bundleFilename := fmt.Sprintf("%s.tar.gz", currentTime.Format(time.RFC3339))
	var tarWriter *tar.Writer
	if a.WriteBundle {
		bundleFile, err := os.Create(path.Join(a.OutputDirectory, bundleFilename))
		if err != nil {
			log.Error().Err(err).Msg("Failed to open file")
		}

		gzipWriter := gzip.NewWriter(bundleFile)
		defer gzipWriter.Close()
		tarWriter = tar.NewWriter(gzipWriter)
		defer tarWriter.Close()
	}

	for cursor.Next(context.TODO()) {
		var realtimeJourney ctdf.RealtimeJourney
		err := cursor.Decode(&realtimeJourney)
		if err != nil {
			log.Error().Err(err).Msg("Failed to decode RealtimeJourney")
		}

		realtimeJourney.GetJourney()
		realtimeJourney.Journey.GetService()

		archivedJourney := a.convertRealtimeToArchiveJourney(&realtimeJourney)
		archivedJourneyJSON, err := json.Marshal(archivedJourney)
		if err != nil {
			log.Error().Err(err).Msg("Error converting archive to json")
		}

		filename := strings.ReplaceAll(fmt.Sprintf("%s.json", archivedJourney.PrimaryIdentifier), "/", "_")

		if a.WriteIndividualFile {
			file, err := os.Create(path.Join(a.OutputDirectory, filename))
			if err != nil {
				log.Error().Err(err).Msg("Failed to open file")
			}

			_, err = file.Write(archivedJourneyJSON)
			if err != nil {
				log.Error().Err(err).Msg("Failed to write to file")
			}

			file.Close()
		}

		if a.WriteBundle {
			memoryFileInfo := MemoryFileInfo{
				MFI_Name:    filename,
				MFI_Size:    int64(len(archivedJourneyJSON)),
				MFI_Mode:    777,
				MFI_ModTime: currentTime,
				MFI_IsDir:   false,
			}

			header, err := tar.FileInfoHeader(memoryFileInfo, filename)
			if err != nil {
				log.Error().Err(err).Msg("Failed to generate tar header")
			}

			// Write file header to the tar archive
			err = tarWriter.WriteHeader(header)
			if err != nil {
				log.Error().Err(err).Msg("Failed to write tar header")
			}

			_, err = tarWriter.Write(archivedJourneyJSON)
			if err != nil {
				log.Error().Err(err).Msg("Failed to write to file")
			}
		}

		recordCount += 1
	}

	log.Info().Int("recordCount", recordCount).Msg("Archive document generation complete")

	if a.CloudUpload {
		a.uploadToStorage(bundleFilename)
	}
}

func (a *Archiver) uploadToStorage(filename string) {
	fullBundlePath := path.Join(a.OutputDirectory, filename)

	client, err := storage.NewClient(context.Background())
	if err != nil {
		log.Fatal().Err(err).Msg("Could not create GCP storage client")
	}

	bucket := client.Bucket(a.CloudBucketName)
	object := bucket.Object(filename)

	reader, err := os.Open(fullBundlePath)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to open file")
	}
	defer reader.Close()

	writer := object.NewWriter(context.Background())
	defer writer.Close()

	io.Copy(writer, reader)

	log.Info().Msgf("Written file %s to bucket %s", object.ObjectName(), object.BucketName())
}

func (a *Archiver) convertRealtimeToArchiveJourney(realtimeJourney *ctdf.RealtimeJourney) *ctdf.ArchivedJourney {
	archivedStops := []*ctdf.ArchivedJourneyStops{}

	stops, arrivalTimes, departureTimes := realtimeJourney.Journey.FlattenStops()

	for _, stopRef := range stops {
		realtimeJourneyStop := realtimeJourney.Stops[stopRef]

		archivedJourneyStop := ctdf.ArchivedJourneyStops{
			StopRef: stopRef,

			ExpectedArrivalTime:   arrivalTimes[stopRef],
			ExpectedDepartureTime: departureTimes[stopRef],

			HasActualData: false,
		}

		if realtimeJourneyStop != nil {
			archivedJourneyStop.HasActualData = true
			archivedJourneyStop.ActualArrivalTime = realtimeJourneyStop.ArrivalTime
			archivedJourneyStop.ActualDepartureTime = realtimeJourneyStop.DepartureTime
		}

		archivedStops = append(archivedStops, &archivedJourneyStop)
	}

	archived := &ctdf.ArchivedJourney{
		PrimaryIdentifier: realtimeJourney.PrimaryIdentifier,

		JourneyRef: realtimeJourney.JourneyRef,

		ServiceRef:  realtimeJourney.Journey.ServiceRef,
		ServiceName: realtimeJourney.Journey.Service.ServiceName,

		OperatorRef: realtimeJourney.Journey.OperatorRef,

		CreationDateTime:     realtimeJourney.CreationDateTime,
		ModificationDateTime: realtimeJourney.ModificationDateTime,

		DataSource: realtimeJourney.DataSource,

		Stops: archivedStops,

		Reliability: realtimeJourney.Reliability,

		VehicleRef: realtimeJourney.VehicleRef,
	}

	return archived
}