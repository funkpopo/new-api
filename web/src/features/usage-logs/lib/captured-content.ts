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
