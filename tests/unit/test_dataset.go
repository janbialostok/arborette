package unit

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/arborette/arborette/internal/dataset/models"
)

func TestDatasetValidation(t *testing.T) {
	// Test valid dataset
	validDataset := &models.Dataset{
		ID:          uuid.New(),
		Name:        "Valid Dataset",
		Description: "This is a valid dataset",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if err := validDataset.Validate(); err != nil {
		t.Errorf("Valid dataset failed validation: %v", err)
	}

	// Test dataset with empty name
	emptyNameDataset := &models.Dataset{
		ID:          uuid.New(),
		Name:        "",
		Description: "This dataset has an empty name",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if err := emptyNameDataset.Validate(); err == nil {
		t.Error("Dataset with empty name should fail validation")
	}

	// Test dataset with name too long
	longName := ""
	for i := 0; i < 256; i++ {
		longName += "a"
	}

	longNameDataset := &models.Dataset{
		ID:          uuid.New(),
		Name:        longName,
		Description: "This dataset has a name that is too long",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if err := longNameDataset.Validate(); err == nil {
		t.Error("Dataset with name too long should fail validation")
	}

	// Test dataset with description too long
	longDescription := ""
	for i := 0; i < 1001; i++ {
		longDescription += "a"
	}

	longDescriptionDataset := &models.Dataset{
		ID:          uuid.New(),
		Name:        "Valid Name",
		Description: longDescription,
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	if err := longDescriptionDataset.Validate(); err == nil {
		t.Error("Dataset with description too long should fail validation")
	}
}