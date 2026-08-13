# Dataset Insights Feature Specification

## Overview
This feature replaces the existing hierarchy with a new model where datasets are the top-level entities, objectives are replaced with questions, and heuristics are replaced with insights. Users can ask questions of datasets, which turn into insights. The system supports easy deletion from each tier with cascading deletions to children. The interface targets an analyst audience rather than data scientists or engineers, with special support for time-series datasets that get updated on a regular cadence.

## User Scenarios
1. User creates a dataset (including time-series datasets)
2. User asks questions about the dataset
3. System generates insights from the questions
4. User analyzes time-series data for periodic changes
5. System automatically generates insights from time-series data changes
6. User deletes a dataset, question, or insight
7. System cascades deletions appropriately

## Functional Requirements
1. Datasets are the top-level entities (replacing previous top-level entity)
2. Questions can be asked about datasets (replacing objectives)
3. Questions generate insights (replacing heuristics)
4. Deletion of datasets cascades to questions and insights
5. Deletion of questions cascades to insights
6. Individual insights can be deleted
7. All entities have unique identifiers
8. All entities maintain creation timestamps
9. Existing functionality is preserved but with new naming and hierarchy
10. User interface is designed for analysts with simplified dashboards and visualizations
11. Time-series datasets support versioning to track changes over time
12. Time-series datasets include trend analysis and comparison tools
13. Data model optimized for time-series with timestamp fields and partitioning
14. Separate time-series optimized tables for efficient storage
15. Line charts, trend graphs, and calendar views for time-series data
16. Automated insight generation from periodic data changes

## Success Criteria
1. Users can create datasets in under 10 seconds
2. Questions generate insights within 30 seconds
3. Deletion operations complete within 5 seconds
4. 95% of dataset operations succeed on first attempt
5. System maintains data integrity during cascading deletions
6. All existing functionality works with new terminology
7. Analysts can easily understand and use the interface without technical expertise
8. Time-series analysis provides meaningful insights about periodic changes

## Key Entities
1. Dataset (replaces previous top-level entity)
   - ID (UUID)
   - Name
   - Description
   - Creation timestamp
   - Time-series flag (for time-series datasets)
   - Update frequency (for time-series datasets)
2. Question (replaces objectives)
   - ID (UUID)
   - Dataset ID (foreign key)
   - Content
   - Creation timestamp
3. Insight (replaces heuristics)
   - ID (UUID)
   - Question ID (foreign key)
   - Content
   - Creation timestamp
   - Time-series context (when generated from time-series data)

## Assumptions
1. Users have appropriate permissions to create/delete entities
2. System has sufficient storage for datasets
3. Questions are text-based queries
4. Insights are generated automatically from questions
5. Deletion is permanent and irreversible
6. System maintains referential integrity
7. Existing functionality will be refactored to use new terminology
8. Database schema will be updated to reflect new naming
9. Analysts prefer simplified interfaces with visualizations over technical details
10. Time-series datasets will be updated regularly (weekly, monthly, etc.)
11. Insights from time-series data changes are more valuable than general dashboard features

## Dependencies
1. Database system for entity storage
2. Text processing system for question analysis
3. Insight generation engine
4. User authentication/authorization system
5. Visualization library for time-series charts and graphs

## Clarifications

### Session 2026-08-12

- Q: Should we refactor existing code or replace it entirely? → A: Refactor existing code to use new terminology while preserving functionality
- Q: Should we redesign data models or maintain existing structures? → A: Redesign data models to better fit the new hierarchy
- Q: Should we refactor database schema along with code? → A: Refactor both code and database schema to use new names
- Q: How should we handle migration to the new model? → A: Direct replacement - immediately switch to new model
- Q: Should user interfaces be updated to use new terms? → A: Update both internal code and user interfaces to new terms

### Session 2026-08-12

- Q: How should the user interface be adjusted to better serve an analyst audience rather than data scientists or engineers? → A: Simplified dashboards with visualizations
- Q: What specific features are needed to support time-series datasets that get updated on a regular cadence (weekly, monthly, etc.)? → A: Versioning system to track changes over time and trend analysis and comparison tools
- Q: How should the data model be adapted to efficiently handle time-series datasets? → A: Implement both timestamp fields and partitioning by time periods and separate time-series optimized tables for flexibility
- Q: What type of visualizations or UI elements would be most helpful for analysts working with time-series datasets? → A: All of the above - Line charts, trend graphs, calendar views, and comparison tools between time periods, specifically tied to insights
- Q: How should the workflow for analyzing time-series datasets differ from regular datasets? → A: Automated insight generation from periodic data changes