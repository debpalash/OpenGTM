import { generatePromptSpec } from "./src/lib/openui-library"
import * as fs from "fs"

const prompt = generatePromptSpec()
fs.writeFileSync("../../data/openui_system_prompt.txt", prompt)
console.log("Wrote OpenUI system prompt to data/openui_system_prompt.txt")
