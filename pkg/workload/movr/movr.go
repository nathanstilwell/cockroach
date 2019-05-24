// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package movr

import (
	gosql "database/sql"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/util/uuid"
	"github.com/cockroachdb/cockroach/pkg/workload"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"golang.org/x/exp/rand"
)

// Indexes into the slice returned by `Tables`.
const (
	TablesUsersIdx                    = 0
	TablesVehiclesIdx                 = 1
	TablesRidesIdx                    = 2
	TablesVehicleLocationHistoriesIdx = 3
	TablesPromoCodesIdx               = 4
	TablesUserPromoCodesIdx           = 5
)

const movrUsersSchema = `(
  id UUID NOT NULL,
  city VARCHAR NOT NULL,
  name VARCHAR NULL,
  address VARCHAR NULL,
  credit_card VARCHAR NULL,
  PRIMARY KEY (city ASC, id ASC)
)`
const movrVehiclesSchema = `(
  id UUID NOT NULL,
  city VARCHAR NOT NULL,
  type VARCHAR NULL,
  owner_id UUID NULL,
  creation_time TIMESTAMP NULL,
  status VARCHAR NULL,
  current_location VARCHAR NULL,
  ext JSONB NULL,
  PRIMARY KEY (city ASC, id ASC),
  INDEX vehicles_auto_index_fk_city_ref_users (city ASC, owner_id ASC)
)`
const movrRidesSchema = `(
  id UUID NOT NULL,
  city VARCHAR NOT NULL,
  vehicle_city VARCHAR NULL,
  rider_id UUID NULL,
  vehicle_id UUID NULL,
  start_address VARCHAR NULL,
  end_address VARCHAR NULL,
  start_time TIMESTAMP NULL,
  end_time TIMESTAMP NULL,
  revenue DECIMAL(10,2) NULL,
  PRIMARY KEY (city ASC, id ASC),
  INDEX rides_auto_index_fk_city_ref_users (city ASC, rider_id ASC),
  INDEX rides_auto_index_fk_vehicle_city_ref_vehicles (vehicle_city ASC, vehicle_id ASC),
  CONSTRAINT check_vehicle_city_city CHECK (vehicle_city = city)
)`
const movrVehicleLocationHistoriesSchema = `(
  city VARCHAR NOT NULL,
  ride_id UUID NOT NULL,
  "timestamp" TIMESTAMP NOT NULL,
  lat FLOAT8 NULL,
  long FLOAT8 NULL,
  PRIMARY KEY (city ASC, ride_id ASC, "timestamp" ASC)
)`
const movrPromoCodesSchema = `(
  code VARCHAR NOT NULL,
  description VARCHAR NULL,
  creation_time TIMESTAMP NULL,
  expiration_time TIMESTAMP NULL,
  rules JSONB NULL,
  PRIMARY KEY (code ASC)
)`
const movrUserPromoCodesSchema = `(
  city VARCHAR NOT NULL,
  user_id UUID NOT NULL,
  code VARCHAR NOT NULL,
  "timestamp" TIMESTAMP NULL,
  usage_count INT NULL,
  PRIMARY KEY (city ASC, user_id ASC, code ASC)
)`

const (
	timestampFormat = "2006-01-02 15:04:05.999999-07:00"
)

var cities = []struct {
	city     string
	locality string
}{
	{city: "new york", locality: "us_east"},
	{city: "boston", locality: "us_east"},
	{city: "washington dc", locality: "us_east"},
	{city: "seattle", locality: "us_west"},
	{city: "san francisco", locality: "us_west"},
	{city: "los angeles", locality: "us_west"},
	{city: "chicago", locality: "us_central"},
	{city: "detroit", locality: "us_central"},
	{city: "minneapolis", locality: "us_central"},
	{city: "amsterdam", locality: "eu_west"},
	{city: "paris", locality: "eu_west"},
	{city: "rome", locality: "eu_west"},
}

type movr struct {
	flags     workload.Flags
	connFlags *workload.ConnFlags

	seed                              uint64
	users, vehicles, rides, histories cityDistributor
	numPromoCodes                     int

	creationTime time.Time
}

func init() {
	workload.Register(movrMeta)
}

var movrMeta = workload.Meta{
	Name:         `movr`,
	Description:  `MovR is a fictional ride sharing company`,
	Version:      `1.0.0`,
	PublicFacing: true,
	New: func() workload.Generator {
		g := &movr{}
		g.flags.FlagSet = pflag.NewFlagSet(`movr`, pflag.ContinueOnError)
		g.flags.Uint64Var(&g.seed, `seed`, 1, `Key hash seed.`)
		g.flags.IntVar(&g.users.numRows, `num-users`, 50, `Initial number of users.`)
		g.flags.IntVar(&g.vehicles.numRows, `num-vehicles`, 15, `Initial number of vehicles.`)
		g.flags.IntVar(&g.rides.numRows, `num-rides`, 500, `Initial number of rides.`)
		g.flags.IntVar(&g.histories.numRows, `num-histories`, 1000,
			`Initial number of ride location histories.`)
		g.flags.IntVar(&g.numPromoCodes, `num-promo-codes`, 1000, `Initial number of promo codes.`)
		g.connFlags = workload.NewConnFlags(&g.flags)
		g.creationTime = time.Date(2019, 1, 2, 3, 4, 5, 6, time.UTC)
		return g
	},
}

// Meta implements the Generator interface.
func (*movr) Meta() workload.Meta { return movrMeta }

// Flags implements the Flagser interface.
func (g *movr) Flags() workload.Flags { return g.flags }

// Hooks implements the Hookser interface.
func (g *movr) Hooks() workload.Hooks {
	return workload.Hooks{
		Validate: func() error {
			// Force there to be at least one user/vehicle/ride/history per city.
			// Otherwise, some cities will be empty, which means we can't construct
			// the FKs we need.
			if g.users.numRows < len(cities) {
				return errors.Errorf(`at least %d users are required`, len(cities))
			}
			if g.vehicles.numRows < len(cities) {
				return errors.Errorf(`at least %d vehicles are required`, len(cities))
			}
			if g.rides.numRows < len(cities) {
				return errors.Errorf(`at least %d rides are required`, len(cities))
			}
			if g.histories.numRows < len(cities) {
				return errors.Errorf(`at least %d histories are required`, len(cities))
			}
			return nil
		},
		PostLoad: func(db *gosql.DB) error {
			fkStmts := []string{
				`ALTER TABLE vehicles ADD FOREIGN KEY ` +
					`(city, owner_id) REFERENCES users (city, id)`,
				`ALTER TABLE rides ADD FOREIGN KEY ` +
					`(city, rider_id) REFERENCES users (city, id)`,
				`ALTER TABLE rides ADD FOREIGN KEY ` +
					`(vehicle_city, vehicle_id) REFERENCES vehicles (city, id)`,
				`ALTER TABLE vehicle_location_histories ADD FOREIGN KEY ` +
					`(city, ride_id) REFERENCES rides (city, id)`,
				`ALTER TABLE user_promo_codes ADD FOREIGN KEY ` +
					`(city, user_id) REFERENCES users (city, id)`,
			}

			for _, fkStmt := range fkStmts {
				if _, err := db.Exec(fkStmt); err != nil {
					// If the statement failed because the fk already exists,
					// ignore it. Return the error for any other reason.
					const duplicateFKErr = "columns cannot be used by multiple foreign key constraints"
					if !strings.Contains(err.Error(), duplicateFKErr) {
						return err
					}
				}
			}

			// TODO(dan): Partitions.
			return nil
		},
	}
}

// Tables implements the Generator interface.
func (g *movr) Tables() []workload.Table {
	tables := make([]workload.Table, 6)
	tables[TablesUsersIdx] = workload.Table{
		Name:   `users`,
		Schema: movrUsersSchema,
		InitialRows: workload.Tuples(
			g.users.numRows,
			g.movrUsersInitialRow,
		),
	}
	tables[TablesVehiclesIdx] = workload.Table{
		Name:   `vehicles`,
		Schema: movrVehiclesSchema,
		InitialRows: workload.Tuples(
			g.vehicles.numRows,
			g.movrVehiclesInitialRow,
		),
	}
	tables[TablesRidesIdx] = workload.Table{
		Name:   `rides`,
		Schema: movrRidesSchema,
		InitialRows: workload.Tuples(
			g.rides.numRows,
			g.movrRidesInitialRow,
		),
	}
	tables[TablesVehicleLocationHistoriesIdx] = workload.Table{
		Name:   `vehicle_location_histories`,
		Schema: movrVehicleLocationHistoriesSchema,
		InitialRows: workload.Tuples(
			g.histories.numRows,
			g.movrVehicleLocationHistoriesInitialRow,
		),
	}
	tables[TablesPromoCodesIdx] = workload.Table{
		Name:   `promo_codes`,
		Schema: movrPromoCodesSchema,
		InitialRows: workload.Tuples(
			g.numPromoCodes,
			g.movrPromoCodesInitialRow,
		),
	}
	tables[TablesUserPromoCodesIdx] = workload.Table{
		Name:   `user_promo_codes`,
		Schema: movrUserPromoCodesSchema,
		InitialRows: workload.Tuples(
			0,
			func(_ int) []interface{} { panic(`unimplemented`) },
		),
	}
	return tables
}

// cityDistributor deterministically maps each of numRows to a city. It also
// maps a city back to a range of rows. This allows the generator functions
// below to select random rows from the same city in another table. numRows is
// required to be at least `len(cities)`.
type cityDistributor struct {
	numRows int
}

func (d cityDistributor) cityForRow(rowIdx int) int {
	if d.numRows < len(cities) {
		panic(errors.Errorf(`a minimum of %d rows are required got %d`, len(cities), d.numRows))
	}
	numPerCity := float64(d.numRows) / float64(len(cities))
	cityIdx := int(float64(rowIdx) / numPerCity)
	return cityIdx
}

func (d cityDistributor) rowsForCity(cityIdx int) (min, max int) {
	if d.numRows < len(cities) {
		panic(errors.Errorf(`a minimum of %d rows are required got %d`, len(cities), d.numRows))
	}
	numPerCity := float64(d.numRows) / float64(len(cities))
	min = int(math.Ceil(float64(cityIdx) * numPerCity))
	max = int(math.Ceil(float64(cityIdx+1) * numPerCity))
	if min >= int(d.numRows) {
		min = int(d.numRows)
	}
	if max >= int(d.numRows) {
		max = int(d.numRows)
	}
	return min, max
}

func (d cityDistributor) randRowInCity(rng *rand.Rand, cityIdx int) int {
	min, max := d.rowsForCity(cityIdx)
	return min + rng.Intn(max-min)
}

func (g *movr) movrUsersInitialRow(rowIdx int) []interface{} {
	rng := rand.New(rand.NewSource(g.seed + uint64(rowIdx)))
	cityIdx := g.users.cityForRow(rowIdx)
	city := cities[cityIdx]

	// Make evenly-spaced UUIDs sorted in the same order as the rows.
	var id uuid.UUID
	id.DeterministicV4(uint64(rowIdx), uint64(g.users.numRows))

	return []interface{}{
		id.String(),         // id
		city.city,           // city
		randName(rng),       // name
		randAddress(rng),    // address
		randCreditCard(rng), // credit_card
	}
}

func (g *movr) movrVehiclesInitialRow(rowIdx int) []interface{} {
	rng := rand.New(rand.NewSource(g.seed + uint64(rowIdx)))
	cityIdx := g.vehicles.cityForRow(rowIdx)
	city := cities[cityIdx]

	// Make evenly-spaced UUIDs sorted in the same order as the rows.
	var id uuid.UUID
	id.DeterministicV4(uint64(rowIdx), uint64(g.vehicles.numRows))

	vehicleType := randVehicleType(rng)
	ownerRowIdx := g.users.randRowInCity(rng, cityIdx)
	ownerID := g.movrUsersInitialRow(ownerRowIdx)[0]

	return []interface{}{
		id.String(),                            // id
		city.city,                              // city
		vehicleType,                            // type
		ownerID,                                // owner_id
		g.creationTime.Format(timestampFormat), // creation_time
		randVehicleStatus(rng),                 // status
		randAddress(rng),                       // current_location
		randVehicleMetadata(rng, vehicleType),  // ext
	}
}

func (g *movr) movrRidesInitialRow(rowIdx int) []interface{} {
	rng := rand.New(rand.NewSource(g.seed + uint64(rowIdx)))
	cityIdx := g.rides.cityForRow(rowIdx)
	city := cities[cityIdx]

	// Make evenly-spaced UUIDs sorted in the same order as the rows.
	var id uuid.UUID
	id.DeterministicV4(uint64(rowIdx), uint64(g.rides.numRows))

	riderRowIdx := g.users.randRowInCity(rng, cityIdx)
	riderID := g.movrUsersInitialRow(riderRowIdx)[0]
	vehicleRowIdx := g.vehicles.randRowInCity(rng, cityIdx)
	vehicleID := g.movrVehiclesInitialRow(vehicleRowIdx)[0]
	startTime := g.creationTime.Add(time.Duration(rng.Intn(30)) * time.Hour)
	endTime := startTime.Add(time.Duration(rng.Intn(30)) * time.Hour)

	return []interface{}{
		id.String(),                       // id
		city.city,                         // city
		city.city,                         // vehicle_city
		riderID,                           // rider_id
		vehicleID,                         // vehicle_id
		randAddress(rng),                  // start_address
		randAddress(rng),                  // end_address
		startTime.Format(timestampFormat), // start_time
		endTime.Format(timestampFormat),   // end_time
		rng.Intn(100),                     // revenue
	}
}

func (g *movr) movrVehicleLocationHistoriesInitialRow(rowIdx int) []interface{} {
	rng := rand.New(rand.NewSource(g.seed + uint64(rowIdx)))
	cityIdx := g.histories.cityForRow(rowIdx)
	city := cities[cityIdx]

	rideRowIdx := g.rides.randRowInCity(rng, cityIdx)
	rideID := g.movrRidesInitialRow(rideRowIdx)[0]
	time := g.creationTime.Add(time.Duration(rowIdx) * time.Millisecond)
	lat, long := float64(-180+rng.Intn(360)), float64(-90+rng.Intn(180))

	return []interface{}{
		city.city,                    // city
		rideID,                       // ride_id,
		time.Format(timestampFormat), // timestamp
		lat,                          // lat
		long,                         // long
	}
}

func (g *movr) movrPromoCodesInitialRow(rowIdx int) []interface{} {
	rng := rand.New(rand.NewSource(g.seed + uint64(rowIdx)))
	codeParts := make([]string, 3)
	for i := range codeParts {
		codeParts[i] = randWord(rng)
	}
	code := fmt.Sprintf(`%s_%d`, strings.Join(codeParts, `_`), rowIdx)
	description := randParagraph(rng)
	expirationTime := g.creationTime.Add(-time.Duration(rng.Intn(30)) * 24 * time.Hour)
	// TODO(dan): This is nil in the reference impl, is that intentional?
	creationTime := expirationTime.Add(-time.Duration(rng.Intn(30)) * 24 * time.Hour)
	const rulesJSON = `{"type": "percent_discount", "value": "10%"}`

	return []interface{}{
		code,           // code
		description,    // description
		creationTime,   // creation_time
		expirationTime, // expiration_time
		rulesJSON,      // rules
	}
}
