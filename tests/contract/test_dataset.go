package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/arborette/arborette/internal/dataset/handlers"
	"github.com/arborette/arborette/internal/dataset/services"
)

// TestDatasetContract tests the dataset creation endpoint contract.
func TestDatasetContract(t *testing.T) {
	// Create a simple in-memory storage for testing
	storage := &inMemoryStorage{
		datasets: make(map[string]*services.Dataset),
	}

	// Create the dataset service
	datasetService := services.NewDatasetService(storage)

	// Create the dataset handler
	datasetHandler := handlers.NewDatasetHandler(datasetService)

	// Create a test dataset
	dataset := &services.Dataset{
		ID:          uuid.New(),
		Name:        "Test Dataset",
		Description: "This is a test dataset",
		CreatedAt:   time.Now().UTC(),
		UpdatedAt:   time.Now().UTC(),
	}

	// Convert the dataset to JSON
	datasetJSON, err := json.Marshal(dataset)
	if err != nil {
		t.Fatalf("Failed to marshal dataset: %v", err)
	}

	// Create a request to the dataset creation endpoint
	req, err := http.NewRequest("POST", "/datasets", bytes.NewBuffer(datasetJSON))
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Create a response recorder
	rr := httptest.NewRecorder()

	// Call the dataset creation handler
	handler := http.HandlerFunc(datasetHandler.CreateDataset)
	handler.ServeHTTP(rr, req)

	// Check the status code
	if status := rr.Code; status != http.StatusCreated {
		t.Errorf("Handler returned wrong status code: got %v want %v", status, http.StatusCreated)
	}

	// Check the response body
	var createdDataset services.Dataset
	if err := json.Unmarshal(rr.Body.Bytes(), &createdDataset); err != nil {
		t.Errorf("Failed to unmarshal response body: %v", err)
	}

	// Check that the created dataset matches the original
	if createdDataset.Name != dataset.Name {
		t.Errorf("Handler returned unexpected name: got %v want %v", createdDataset.Name, dataset.Name)
	}

	if createdDataset.Description != dataset.Description {
		t.Errorf("Handler returned unexpected description: got %v want %v", createdDataset.Description, dataset.Description)
	}
}

// Simple in-memory storage implementation for testing
type inMemoryStorage struct {
	datasets map[string]*services.Dataset
}

func (s *inMemoryStorage) CreateDataset(ctx context.Context, dataset *services.Dataset) error {
	s.datasets[dataset.ID.String()] = dataset
	return nil
}

func (s *inMemoryStorage) GetDataset(ctx context.Context, id uuid.UUID) (*services.Dataset, error) {
	dataset, exists := s.datasets[id.String()]
	if !exists {
		return nil, nil
	}
	return dataset, nil
}

func (s *inMemoryStorage) UpdateDataset(ctx context.Context, dataset *services.Dataset) error {
	s.datasets[dataset.ID.String()] = dataset
	return nil
}

func (s *inMemoryStorage) DeleteDataset(ctx context.Context, id uuid.UUID) error {
	delete(s.datasets, id.String())
	return nil
}

func (s *inMemoryStorage) ListDatasets(ctx context.Context) ([]*services.Dataset, error) {
	datasets := make([]*services.Dataset, 0, len(s.datasets))
	for _, dataset := range s.datasets {
		datasets = append(datasets, dataset)
	}
	return datasets, nil
}