---
intent_links:
  - intent: "#tasks"
    code:
      - Justfile
      - tasks_test.go:TestTaskRecipes
---
# Tasks

Tasks are orchestrated via a Justfile.

Task list:

- build: local build
- install: local install
- release: invokes release script