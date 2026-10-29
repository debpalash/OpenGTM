import { expect, test } from "bun:test"
import { parseRecordJSON } from "../src/lib/record-json"
test("large external identifiers retain their exact digits in record views", () => {
  expect(
    parseRecordJSON(
      '{"external":9007199254740993,"nested":{"negative":-9007199254740993},"score":55}'
    )
  ).toEqual({
    external: "9007199254740993",
    nested: { negative: "-9007199254740993" },
    score: 55
  })
})
test("safe numeric, boolean, and object fields retain their types", () =>
  expect(parseRecordJSON('{"n":123,"x":1.25,"b":false,"a":[1,null]}')).toEqual({
    n: 123,
    x: 1.25,
    b: false,
    a: [1, null]
  }))
