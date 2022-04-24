package routes

import (
	"context"

	"github.com/britbus/britbus/pkg/ctdf"
	"github.com/britbus/britbus/pkg/database"
	"github.com/gofiber/fiber/v2"
	"go.mongodb.org/mongo-driver/bson"
)

func Stats(c *fiber.Ctx) error {
	stopsCollection := database.GetCollection("stops")
	numberStops, _ := stopsCollection.CountDocuments(context.Background(), bson.D{})

	operatorsCollection := database.GetCollection("operators")
	numberOperators, _ := operatorsCollection.CountDocuments(context.Background(), bson.D{})

	servicesCollection := database.GetCollection("services")
	numberServices, _ := servicesCollection.CountDocuments(context.Background(), bson.D{})

	realtimeJourneysCollection := database.GetCollection("realtime_journeys")

	numberRealtimeJourneys, _ := realtimeJourneysCollection.CountDocuments(context.Background(), bson.D{})

	var numberActiveRealtimeJourneys int64
	realtimeActiveCutoffDate := ctdf.GetActiveRealtimeJourneyCutOffDate()
	activeRealtimeJourneys, _ := realtimeJourneysCollection.Find(context.Background(), bson.M{
		"modificationdatetime": bson.M{"$gt": realtimeActiveCutoffDate},
	})
	for activeRealtimeJourneys.Next(context.TODO()) {
		var realtimeJourney *ctdf.RealtimeJourney
		activeRealtimeJourneys.Decode(&realtimeJourney)

		if realtimeJourney.IsActive() {
			numberActiveRealtimeJourneys += 1
		}
	}

	numberHistoricRealtimeJourneys := numberRealtimeJourneys - numberActiveRealtimeJourneys

	return c.JSON(fiber.Map{
		"stops":                        numberStops,
		"operators":                    numberOperators,
		"services":                     numberServices,
		"active_realtime_journeys":     numberActiveRealtimeJourneys,
		"historical_realtime_journeys": numberHistoricRealtimeJourneys,
	})
}
