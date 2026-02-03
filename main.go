package main

import (
	"log"
	"os"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// --- 1. DATA MODELS (Mapping your Schema to Go) ---

type Customer struct {
	ID            uint      `json:"id" gorm:"primaryKey"`
	Phone         string    `json:"phone" gorm:"unique;not null"`
	Name          string    `json:"name" gorm:"not null"`
	LoyaltyPoints int       `json:"loyalty_points" gorm:"default:0"`
	CreatedAt     time.Time `json:"created_at"`
}

type InventoryItem struct {
	ID           uint    `json:"id" gorm:"primaryKey"`
	Name         string  `json:"name"`
	Type         string  `json:"type"` // 'rental' or 'consumable'
	CurrentPrice float64 `json:"current_price"`
	IsActive     bool    `json:"is_active" gorm:"default:true"`
}

type Booking struct {
	ID             uint      `json:"id" gorm:"primaryKey"`
	CustomerID     uint      `json:"customer_id"`
	Customer       Customer  `json:"customer" gorm:"foreignKey:CustomerID"` // For populating customer details
	CourtNumber    int       `json:"court_number"`
	BookingDate    string    `json:"booking_date"` // Storing as string YYYY-MM-DD for simplicity in JSON
	StartTime      string    `json:"start_time"`   // HH:MM:SS
	EndTime        string    `json:"end_time"`
	CourtPrice     float64   `json:"court_price" gorm:"default:0"`
	ItemsTotal     float64   `json:"items_total" gorm:"default:0"`
	DiscountAmount float64   `json:"discount_amount" gorm:"default:0"`
	FinalTotal     float64   `json:"final_total"`
	PaymentStatus  string    `json:"payment_status" gorm:"default:'PENDING'"`
	CreatedAt      time.Time `json:"created_at"`

	// Relationship: One Booking has Many Items
	// "cascade:OnDelete" ensures if Booking is deleted, items are deleted too (keeping DB clean)
	Items []BookingItem `json:"items" gorm:"foreignKey:BookingID;constraint:OnDelete:CASCADE"`
}

type BookingItem struct {
	ID             uint          `json:"id" gorm:"primaryKey"`
	BookingID      uint          `json:"booking_id"`
	ItemID         uint          `json:"item_id"`
	InventoryItem  InventoryItem `json:"inventory_details" gorm:"foreignKey:ItemID"` // To see name of item
	Quantity       int           `json:"quantity"`
	PriceAtBooking float64       `json:"price_at_booking"`
}

// Global DB variable
var db *gorm.DB

func main() {
	// --- 2. DATABASE CONNECTION ---
	dsn := os.Getenv("DATABASE_URL")
	// If testing locally, you can hardcode it (but REMOVE before deploying):
	// dsn = "host=localhost user=postgres password=password dbname=courtdb port=5432 sslmode=disable"

	var err error
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatal("Failed to connect to database: ", err)
	}

	// Migrate schemas (Create tables automatically if they don't exist)
	db.AutoMigrate(&Customer{}, &InventoryItem{}, &Booking{}, &BookingItem{})

	// Initialize App
	app := fiber.New()
	app.Use(cors.New()) // Allow frontend to call this API

	// --- 3. ROUTES ---

	// POST /bookings - Create a booking (and booking_items if present)
	app.Post("/bookings", createBooking)

	// GET /bookings - Fetch bookings filtered by date
	app.Get("/bookings", getBookings)

	// PATCH /bookings/:id - Update status or times
	app.Patch("/bookings/:id", updateBooking)

	// DELETE /bookings/:id - Remove booking
	app.Delete("/bookings/:id", deleteBooking)

	// Start Server
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	log.Fatal(app.Listen(":" + port))
}

// --- 4. HANDLERS (The Logic) ---

// GET: Fetch bookings for a specific date
func getBookings(c *fiber.Ctx) error {
	// 1. Get the 'date' parameter from URL (e.g., ?booking_date=2023-10-27)
	dateParam := c.Query("booking_date")

	if dateParam == "" {
		return c.Status(400).JSON(fiber.Map{"error": "Please provide a booking_date parameter"})
	}

	var bookings []Booking

	// 2. Query DB: Filter by date AND Preload related data (Items and Customer name)
	// 'Preload' is GORM magic. It does the JOINs for you so your JSON includes the nested items.
	result := db.Preload("Items").Preload("Items.InventoryItem").Preload("Customer").Where("booking_date = ?", dateParam).Find(&bookings)

	if result.Error != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Could not fetch bookings"})
	}

	return c.JSON(bookings)
}

// POST: Create a new booking
func createBooking(c *fiber.Ctx) error {
	booking := new(Booking)

	// Parse JSON input
	if err := c.BodyParser(booking); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid Input"})
	}

	if booking.Customer.Phone != "" {
		var existingCust Customer

		// Search DB for this phone number
		if err := db.Where("phone = ?", booking.Customer.Phone).First(&existingCust).Error; err == nil {
			// SCENARIO A: Customer Found!
			// We link this new booking to the EXISTING customer's ID
			booking.CustomerID = existingCust.ID

			// CRITICAL: We must empty the 'Customer' struct.
			// If we don't, GORM will try to create the customer again and fail
			// because the phone number must be unique.
			booking.Customer = Customer{}
		}
		// SCENARIO B: Customer Not Found (err != nil)
		// We do nothing. GORM will see the new Customer data and automatically
		// create the new customer row for us.
	}

	// LOGIC: Calculate Item Prices
	// We must look up the current price of items to set 'PriceAtBooking'
	var itemsTotal float64 = 0
	for i, item := range booking.Items {
		var invItem InventoryItem
		if err := db.First(&invItem, item.ItemID).Error; err == nil {
			// Set the snapshot price
			booking.Items[i].PriceAtBooking = invItem.CurrentPrice
			itemsTotal += invItem.CurrentPrice * float64(item.Quantity)
		}
	}

	booking.ItemsTotal = itemsTotal
	// Simple Total calculation (You can add logic for CourtPrice here)
	booking.FinalTotal = booking.CourtPrice + booking.ItemsTotal - booking.DiscountAmount

	// Save to DB (GORM saves the Booking AND the nested BookingItems automatically)
	if err := db.Create(&booking).Error; err != nil {
		return c.Status(500).JSON(fiber.Map{"error": "Could not save booking"})
	}

	return c.Status(201).JSON(booking)
}

// PATCH: Update a booking
func updateBooking(c *fiber.Ctx) error {
	id := c.Params("id")
	var booking Booking

	// Find the booking first
	if err := db.First(&booking, id).Error; err != nil {
		return c.Status(404).JSON(fiber.Map{"error": "Booking not found"})
	}

	// Structure to hold data we want to update
	type UpdateInput struct {
		StartTime     string  `json:"start_time"`
		EndTime       string  `json:"end_time"`
		PaymentStatus string  `json:"payment_status"`
		CourtPrice    float64 `json:"court_price"`
	}

	var input UpdateInput
	if err := c.BodyParser(&input); err != nil {
		return c.Status(400).JSON(fiber.Map{"error": "Invalid Input"})
	}

	// Update only the fields provided
	db.Model(&booking).Updates(input)

	return c.JSON(booking)
}

// DELETE: Remove a booking
func deleteBooking(c *fiber.Ctx) error {
	id := c.Params("id")

	// Delete the booking (Cascade will handle items if configured in DB, otherwise GORM handles it)
	// Uses 'Unscoped' to permanently delete (hard delete). Remove 'Unscoped()' for soft delete.
	result := db.Unscoped().Delete(&Booking{}, id)

	if result.RowsAffected == 0 {
		return c.Status(404).JSON(fiber.Map{"error": "Booking not found"})
	}

	return c.JSON(fiber.Map{"message": "Booking deleted successfully"})
}
