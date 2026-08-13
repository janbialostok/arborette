# Research for Dataset Insights Feature

## Technical Decisions

### Language Choice
**Decision**: Go 1.24
**Rationale**: The project is already using Go as its primary language, and Go 1.24 is the pinned version in the go.mod file. This ensures consistency with existing codebase and tooling.

### Database Choices
**Decision**: Neo4j for relationships, PostgreSQL with pgvector for embeddings, specialized time-series tables
**Rationale**: 
- Neo4j is well-suited for handling the graph-like relationships between datasets, questions, and insights
- PostgreSQL with pgvector is already used in the project for storing embeddings
- Specialized time-series tables will provide better performance for time-series data queries
- This approach leverages existing infrastructure and expertise while adding specialized storage for time-series data

### Storage Approach
**Decision**: Separate storage for relationships, embeddings, and time-series data
**Rationale**: 
- Relationships between entities (dataset → question → insight) are best modeled in a graph database
- Embeddings (if needed for semantic search) are best stored in a vector database
- Time-series data requires specialized storage for efficient querying and analysis
- This separation allows for optimized storage and querying for each use case

### Testing Framework
**Decision**: Go testing package with testify
**Rationale**: 
- Consistent with existing project testing approach
- testify provides useful assertion utilities
- No additional dependencies required

### API Design
**Decision**: RESTful API
**Rationale**: 
- Consistent with existing project patterns
- Well-understood by developers
- Good tooling support

### Visualization Library
**Decision**: Use Plotly.js for interactive charts
**Rationale**: 
- Well-established library with good documentation
- Supports various chart types including line charts and trend graphs
- Good integration with web applications
- Suitable for analyst-focused interfaces

### Time-Series Data Handling
**Decision**: Implement versioning system with time-based partitioning
**Rationale**: 
- Versioning allows tracking changes over time
- Time-based partitioning improves query performance for time-series data
- Supports the requirement for regular (weekly/monthly) updates

## Alternatives Considered

### Single Database vs Separate Databases
- Alternative: Use only PostgreSQL for all storage needs
- Rejected because: Neo4j provides better performance for graph traversals and relationship queries, and specialized time-series tables are needed for efficient time-series data handling

### GraphQL vs REST
- Alternative: Use GraphQL for the API
- Rejected because: REST is simpler for this use case and consistent with existing project patterns

### Different Visualization Libraries
- Alternative: Use D3.js for more customizable visualizations
- Rejected because: Plotly.js provides sufficient functionality with less complexity, which is better for analyst-focused interfaces

### Different Language
- Alternative: Use Python for better data science libraries
- Rejected because: Go is already established in the project and provides good performance characteristics, and the existing codebase is in Go