package main

import (
	"strings"
	"testing"
)

// TestGenAPIMarkdown: operations grouped by tag, with their parameters,
// request bodies and responses, then every schema with its fields;
// allOf parts are flattened into one table.
func TestGenAPIMarkdown(t *testing.T) {
	md, err := genAPIMarkdown([]byte(testOpenAPI))
	if err != nil {
		t.Fatal(err)
	}
	got := string(md)
	for _, want := range []string{
		"# Example API",
		"Intro text.",
		"## things",
		"Thing operations.",
		"### `GET /things/{id}`",
		"**Get a thing.** Returns it.",
		"| `id` | path | integer | yes | The id. |",
		"| `verbose` | query | boolean | no | — |",
		"| 200 | `application/json` | [Thing](#thing) | Found. |",
		"| 404 | `application/json` | [Error](#error) | An error. |",
		"Auth: `token` (bearer)",
		"### `POST /things`",
		"Request body (`application/json`): [Thing](#thing)",
		"| 202 | — | — | Accepted. |",
		"### Thing",
		"A thing.",
		"| `id` | integer | yes | — |",
		"| `tags` | array of string | no | Labels. |",
		"| `kind` | `a` \\| `b` | no | — |",
		"| `parent` | [Thing](#thing) or null | no | — |",
		"### Big",
		"| `extra` | map of [Thing](#thing) | no | — |",
		"| `name` | string | yes | — |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("api.md lacks %q:\n%s", want, got)
		}
	}
}

const testOpenAPI = `openapi: 3.1.0
info:
  title: Example API
  version: v1
  description: Intro text.
tags:
  - name: things
    description: Thing operations.
security:
  - token: []
paths:
  /things/{id}:
    parameters:
      - name: id
        in: path
        required: true
        description: The id.
        schema: {type: integer}
    get:
      tags: [things]
      summary: Get a thing.
      description: Returns it.
      parameters:
        - name: verbose
          in: query
          schema: {type: boolean}
      responses:
        "200":
          description: Found.
          content:
            application/json:
              schema: {$ref: "#/components/schemas/Thing"}
        "404": {$ref: "#/components/responses/Error"}
  /things:
    post:
      tags: [things]
      summary: Make a thing.
      requestBody:
        content:
          application/json:
            schema: {$ref: "#/components/schemas/Thing"}
      responses:
        "202":
          description: Accepted.
components:
  securitySchemes:
    token: {type: http, scheme: bearer, description: A token.}
  responses:
    Error:
      description: An error.
      content:
        application/json:
          schema: {$ref: "#/components/schemas/Error"}
  schemas:
    Error:
      type: object
      properties:
        error: {type: string}
    Thing:
      type: object
      description: A thing.
      required: [id]
      properties:
        id: {type: integer}
        tags: {type: array, items: {type: string}, description: Labels.}
        kind: {type: string, enum: [a, b]}
        parent:
          anyOf:
            - {$ref: "#/components/schemas/Thing"}
            - {type: "null"}
    Big:
      allOf:
        - {$ref: "#/components/schemas/Named"}
        - type: object
          properties:
            extra:
              type: object
              additionalProperties: {$ref: "#/components/schemas/Thing"}
    Named:
      type: object
      required: [name]
      properties:
        name: {type: string}
`
