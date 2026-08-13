# Quickstart Guide for Dataset Insights

## Prerequisites

1. Go 1.24 installed
2. Neo4j database running
3. PostgreSQL database with pgvector extension running
4. Environment variables set:
   - `NEO4J_URI` - Neo4j connection URI
   - `NEO4J_USER` - Neo4j username
   - `NEO4J_PASSWORD` - Neo4j password
   - `POSTGRES_URI` - PostgreSQL connection URI

## Setup

1. Clone the repository:
   ```bash
   git clone <repository-url>
   cd arborette
   ```

2. Install dependencies:
   ```bash
   go mod tidy
   ```

3. Set up databases:
   - Start Neo4j and PostgreSQL services
   - Run database migrations to rename tables for new terminology

## Running Services

1. Start the dataset service:
   ```bash
   go run cmd/dataset-service/main.go
   ```

2. Start the question service:
   ```bash
   go run cmd/question-service/main.go
   ```

3. Start the insight service:
   ```bash
   go run cmd/insight-service/main.go
   ```

4. Start the time-series service:
   ```bash
   go run cmd/timeseries-service/main.go
   ```

5. Start the visualization service:
   ```bash
   go run cmd/visualization-service/main.go
   ```

## Testing the API

### Create a Time-Series Dataset

```bash
curl -X POST http://localhost:8080/datasets \
  -H "Content-Type: application/json" \
  -d '{"name": "Transaction Fraud Data", "description": "Weekly transaction fraud monitoring", "isTimeSeries": true, "updateFrequency": "weekly"}'
```

### Create a Dataset Version

```bash
curl -X POST http://localhost:8080/datasets/{datasetId}/versions \
  -H "Content-Type: application/json" \
  -d '{"dataSnapshot": {"week_start": "2026-08-03", "total_transactions": 15234, "fraud_count": 127, "fraud_rate": 0.83}}'
```

### Create a Question About Time-Series Data

```bash
curl -X POST http://localhost:8081/questions \
  -H "Content-Type: application/json" \
  -d '{"datasetId": "dataset-uuid-here", "content": "Why did fraud rates increase this week compared to last week?"}'
```

### Generate a Time-Series Insight

```bash
curl -X POST http://localhost:8082/insights \
  -H "Content-Type: application/json" \
  -d '{"questionId": "question-uuid-here", "content": "Fraud detection rate is continuously increasing at a rate of 0.5% per week. This trend started 4 weeks ago and correlates with the introduction of new payment methods.", "isTimeSeriesInsight": true}'
```

### Compare Dataset Versions

```bash
curl http://localhost:8080/timeseries/{datasetId}/compare?version1=1&version2=2
```

### Get Trend Analysis

```bash
curl http://localhost:8080/timeseries/{datasetId}/trends?startDate=2026-01-01&endDate=2026-08-12
```

### Test Cascading Deletion

1. Delete a dataset:
   ```bash
   curl -X DELETE http://localhost:8080/datasets/dataset-uuid-here
   ```

2. Verify that associated questions, insights, and versions are also deleted.

## Analyst-Focused UI

The web interface at `http://localhost:3000` provides a simplified dashboard designed for analysts:

1. Dataset Overview: See all your datasets with clear time-series indicators
2. Question Builder: Ask questions using natural language about your datasets
3. Insight Feed: Review automatically generated insights with trend visualizations
4. Time-Series Analysis: View trend graphs and time-period comparisons
5. Cascade Management: Easily delete datasets, questions, or insights with cascading deletions

## Running Tests

1. Run unit tests:
   ```bash
   go test ./...
   ```

2. Run integration tests (requires running databases):
   ```bash
   ARBORETTE_INTEGRATION=1 go test ./...
   ```

## Validation Criteria

1. All API endpoints respond within 200ms for read operations
2. All API endpoints respond within 500ms for write operations
3. Dataset creation completes in under 10 seconds
4. Question to insight generation completes within 30 seconds (including time-series analysis)
5. Deletion operations complete within 5 seconds with cascading
6. 95% of dataset operations succeed on first attempt
7. System maintains data integrity during cascading deletions
8. Analysts can easily understand and use the interface without technical expertise
9. Time-series analysis provides meaningful insights about periodic changes
10. Trend analysis correctly identifies important patterns in time-series data