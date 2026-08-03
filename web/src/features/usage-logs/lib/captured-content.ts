/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact [邮箱]
*/
function decodeBase64Chunks(chunks: string[]): Uint8Array {
  const decodedChunks = chunks.map((chunk) => {
    const binary = atob(chunk)
    const bytes = new Uint8Array(binary.length)
    for (let index = 0; index < binary.length; index++) {
      bytes[index] = binary.charCodeAt(index)
    }
    return bytes
  })
  const totalLength = decodedChunks.reduce(
    (total, chunk) => total + chunk.length,
    0
  )
  const combined = new Uint8Array(totalLength)
  let offset = 0
  for (const chunk of decodedChunks) {
    combined.set(chunk, offset)
    offset += chunk.length
  }
  return combined
}

export function isTextContentType(contentType: string): boolean {
  const normalized = contentType.toLowerCase().split(';', 1)[0].trim()
  return (
    normalized === '' ||
    normalized.startsWith('text/') ||
    normalized.includes('json') ||
    normalized.includes('xml') ||
    normalized.includes('javascript') ||
    normalized === 'application/x-www-form-urlencoded'
  )
}

function encodeBase64(bytes: Uint8Array): string {
  const blocks: string[] = []
  for (let offset = 0; offset < bytes.length; offset += 32_768) {
    blocks.push(String.fromCharCode(...bytes.subarray(offset, offset + 32_768)))
  }
  return btoa(blocks.join(''))
}

export function decodeCapturedContent(
  chunks: string[],
  contentType: string
): string {
  const bytes = decodeBase64Chunks(chunks)
  if (isTextContentType(contentType)) {
    return new TextDecoder().decode(bytes)
  }
  return encodeBase64(bytes)
}

export type CapturedContentPresentation = {
  content: string
  renderMarkdown: boolean
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function readTextContent(value: unknown): string | null {
  if (typeof value === 'string') return value
  if (!Array.isArray(value)) return null

  const parts: string[] = []
  for (const part of value) {
    if (!isRecord(part)) continue
    if (typeof part.text === 'string') {
      parts.push(part.text)
    } else if (typeof part.content === 'string') {
      parts.push(part.content)
    }
  }
  return parts.length > 0 ? parts.join('') : null
}

function readChatCompletionContent(
  value: Record<string, unknown>
): string | null {
  if (!Array.isArray(value.choices)) return null

  const parts: string[] = []
  let foundContent = false
  for (const choice of value.choices) {
    if (!isRecord(choice)) continue
    let container = choice
    if (isRecord(choice.delta)) {
      container = choice.delta
    } else if (isRecord(choice.message)) {
      container = choice.message
    }
    const content = readTextContent(container.content)
    if (content !== null) {
      foundContent = true
      parts.push(content)
    }
  }
  return foundContent ? parts.join('') : null
}

function readResponsesContent(value: Record<string, unknown>): string | null {
  if (typeof value.output_text === 'string') return value.output_text
  if (!Array.isArray(value.output)) return null

  const parts: string[] = []
  for (const item of value.output) {
    if (!isRecord(item)) continue
    const content = readTextContent(item.content)
    if (content !== null) parts.push(content)
  }
  return parts.length > 0 ? parts.join('') : null
}

function readResponseContent(value: unknown): string | null {
  if (!isRecord(value)) return null
  return readChatCompletionContent(value) ?? readResponsesContent(value)
}

function readRequestItemsContent(value: unknown): string | null {
  if (!Array.isArray(value)) return null

  const parts: string[] = []
  for (const item of value) {
    if (typeof item === 'string') {
      parts.push(item)
      continue
    }
    if (!isRecord(item)) continue

    const content = readTextContent(item.content)
    if (content !== null) parts.push(content)
  }
  return parts.length > 0 ? parts.join('\n\n') : null
}

function readRequestContent(value: unknown): string | null {
  if (!isRecord(value)) return null

  const messagesContent = readRequestItemsContent(value.messages)
  if (messagesContent !== null) return messagesContent

  if (typeof value.input === 'string') return value.input
  const inputContent = readRequestItemsContent(value.input)
  if (inputContent !== null) return inputContent

  return readTextContent(value.content)
}

function readSsePayloadContent(value: unknown): string | null {
  if (!isRecord(value)) return null

  const chatContent = readChatCompletionContent(value)
  if (chatContent !== null) return chatContent

  if (
    value.type === 'response.output_text.delta' &&
    typeof value.delta === 'string'
  ) {
    return value.delta
  }
  return null
}

type ParsedSseEvent = {
  complete: boolean
  content: string | null
}

function parseSseEvent(dataLines: string[]): ParsedSseEvent {
  if (dataLines.length === 0) return { complete: true, content: null }

  const payload = dataLines.join('\n')
  if (payload === '[DONE]') return { complete: true, content: null }

  const value = parseJson(payload)
  if (value === null) return { complete: false, content: null }
  return { complete: true, content: readSsePayloadContent(value) }
}

function extractSseContent(content: string): string | null {
  const eventStream = /^\s*(?:event:|data:)/m.test(content)
  if (!eventStream) return null

  const parts: string[] = []
  let dataLines: string[] = []
  const lines = content
    .replaceAll('\r\n', '\n')
    .replaceAll('\r', '\n')
    .split('\n')
  for (const line of lines) {
    if (line.startsWith('data:')) {
      const previousEvent = parseSseEvent(dataLines)
      if (dataLines.length > 0 && previousEvent.complete) {
        if (previousEvent.content !== null) parts.push(previousEvent.content)
        dataLines = []
      }
      dataLines.push(line.slice(5).trimStart())
      continue
    }

    if (line === '') {
      const event = parseSseEvent(dataLines)
      if (event.complete && event.content !== null) parts.push(event.content)
      dataLines = []
    }
  }

  const finalEvent = parseSseEvent(dataLines)
  if (finalEvent.complete && finalEvent.content !== null) {
    parts.push(finalEvent.content)
  }
  return parts.join('')
}

function parseJson(content: string): unknown | null {
  try {
    return JSON.parse(content)
  } catch {
    return null
  }
}

export function formatCapturedContent(
  content: string,
  contentType: string,
  kind: 'request' | 'response'
): CapturedContentPresentation {
  if (kind === 'response') {
    const streamedContent = extractSseContent(content)
    if (streamedContent !== null) {
      return { content: streamedContent, renderMarkdown: true }
    }

    const parsed = parseJson(content)
    const responseContent = readResponseContent(parsed)
    if (responseContent !== null) {
      return { content: responseContent, renderMarkdown: true }
    }
    if (parsed !== null) {
      return {
        content: JSON.stringify(parsed, null, 2),
        renderMarkdown: false,
      }
    }

    return {
      content,
      renderMarkdown: isTextContentType(contentType),
    }
  }

  const parsed = parseJson(content)
  const requestContent = readRequestContent(parsed)
  if (requestContent !== null) {
    return { content: requestContent, renderMarkdown: true }
  }
  return {
    content: parsed === null ? content : JSON.stringify(parsed, null, 2),
    renderMarkdown: false,
  }
}
