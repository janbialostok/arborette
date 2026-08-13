# Data Model for Dataset Insights

## Entities

### Dataset
Represents a collection of data that users can query. Can be either a regular dataset or a time-series dataset.

**Fields:**
- ID (UUID) - Primary key, unique identifier
- Name (string) - Human-readable name for the dataset
- Description (string) - Detailed description of the dataset
- CreatedAt (timestamp) - When the dataset was created
- UpdatedAt (timestamp) - When the dataset was last updated
- IsTimeSeries (boolean) - Flag indicating if this is a time-series dataset
- UpdateFrequency (string) - For time-series datasets, how often the data is updated (weekly, monthly, etc.)
- LastUpdated (timestamp) - For time-series datasets, when the data was last updated

**Relationships:**
- Has many Questions (one-to-many)
- Has many Versions (one-to-many, for time-series datasets)

### DatasetVersion
Represents a version of a time-series dataset at a specific point in time.

**Fields:**
- ID (UUID) - Primary key, unique identifier
- DatasetID (UUID) - Foreign key to Dataset
- VersionNumber (integer) - Sequential version number
- CreatedAt (timestamp) - When this version was created
- DataSnapshot (JSON) - Snapshot of the dataset at this point in time

**Relationships:**
- Belongs to Dataset (many-to-one)

### Question
Represents a query about a dataset.

**Fields:**
- ID (UUID) - Primary key, unique identifier
- DatasetID (UUID) - Foreign key to Dataset
- Content (string) - The text of the question
- CreatedAt (timestamp) - When the question was created
- UpdatedAt (timestamp) - When the question was last updated

**Relationships:**
- Belongs to Dataset (many-to-one)
- Has many Insights (one-to-many)

### Insight
Represents an insight generated from a question.

**Fields:**
- ID (UUID) - Primary key, unique identifier
- QuestionID (UUID) - Foreign key to Question
- Content (string) - The text of the insight
- CreatedAt (timestamp) - When the insight was created
- UpdatedAt (timestamp) - When the insight was last updated
- IsTimeSeriesInsight (boolean) - Flag indicating if this insight was generated from time-series data
- TimeSeriesContext (JSON) - Additional context for time-series insights

**Relationships:**
- Belongs to Question (many-to-one)

## Validation Rules

1. Dataset name must be between 1 and 255 characters
2. Dataset description must be less than 1000 characters
3. Question content must be between 1 and 1000 characters
4. Insight content must be between 1 and 5000 characters
5. All timestamps must be in UTC
6. All IDs must be valid UUIDs
7. UpdateFrequency must be one of: "daily", "weekly", "monthly", "quarterly", "yearly"
8. VersionNumber must be a positive integer

## Cascade Deletion Rules

1. When a Dataset is deleted:
   - All associated Questions are deleted
   - All Insights associated with those Questions are deleted
   - All DatasetVersions are deleted

2. When a Question is deleted:
   - All associated Insights are deleted

3. When an Insight is deleted:
   - No cascading deletion occurs

## Indexes

1. Dataset:
   - Primary index on ID
   - Index on Name for faster lookups
   - Index on IsTimeSeries for filtering time-series datasets
   - Index on LastUpdated for time-series analysis

2. DatasetVersion:
   - Primary index on ID
   - Index on DatasetID for faster lookups
   - Index on VersionNumber for ordering
   - Index on CreatedAt for time-based queries

3. Question:
   - Primary index on ID
   - Index on DatasetID for faster lookups
   - Index on CreatedAt for sorting

4. Insight:
   - Primary index on ID
   - Index on QuestionID for faster lookups
   - Index on CreatedAt for sorting
   - Index on IsTimeSeriesInsight for filtering time-series insights