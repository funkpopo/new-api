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
import assert from 'node:assert/strict'
import { describe, test } from 'node:test'

import {
  decodeCapturedContent,
  formatCapturedContent,
} from '../../lib/captured-content'

function toBase64(bytes: Uint8Array): string {
  return Buffer.from(bytes).toString('base64')
}

describe('captured usage-log content', () => {
  test('reassembles complete UTF-8 content split inside a multibyte character', () => {
    const original = 'request: 完整内容 response: ✓'
    const bytes = new TextEncoder().encode(original)
    const splitIndex = bytes.indexOf(0xe5) + 1
    const chunks = [
      toBase64(bytes.subarray(0, splitIndex)),
      toBase64(bytes.subarray(splitIndex)),
    ]

    assert.equal(
      decodeCapturedContent(chunks, 'application/json; charset=utf-8'),
      original
    )
  })

  test('preserves arbitrary binary content as a single Base64 value', () => {
    const bytes = Uint8Array.from([0, 255, 1, 254, 2, 253])
    const chunks = [
      toBase64(bytes.subarray(0, 2)),
      toBase64(bytes.subarray(2, 5)),
      toBase64(bytes.subarray(5)),
    ]

    assert.equal(decodeCapturedContent(chunks, 'audio/mpeg'), toBase64(bytes))
  })

  test('shows only accumulated content from an OpenAI-compatible stream', () => {
    const captured = [
      'data: {"id":"chunk-1","choices":[{"index":0,"delta":{"reasoning_content":"hidden"}}]}',
      '',
      'data: {"id":"chunk-1","choices":[{"index":0,"delta":{"content":"## Result\\n\\n"}}]}',
      '',
      'data: {"id":"chunk-1","choices":[{"index":0,"delta":{"content":"**ready**"}}]}',
      '',
      'data: {"id":"chunk-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}',
      '',
      'data: [DONE]',
      '',
    ].join('\n')

    assert.deepEqual(
      formatCapturedContent(captured, 'text/event-stream', 'response'),
      {
        content: '## Result\n\n**ready**',
        renderMarkdown: true,
      }
    )
  })

  test('concatenates token fragments when SSE events have no blank separator', () => {
    const captured = [
      'data: {"choices":[{"index":0,"delta":{"content":"服务"}}]}',
      'data: {"choices":[{"index":0,"delta":{"content":"已"}}]}',
      'data: {"choices":[{"index":0,"delta":{"content":"成功"}}]}',
      'data: {"choices":[{"index":0,"delta":{"content":"启动"}}]}',
      'data: {"choices":[{"index":0,"delta":{"content":"。\\n下一行"}}]}',
      'data: [DONE]',
    ].join('\r\n')

    assert.deepEqual(
      formatCapturedContent(captured, 'text/event-stream', 'response'),
      {
        content: '服务已成功启动。\n下一行',
        renderMarkdown: true,
      }
    )
  })

  test('shows native Responses output without duplicating the completed response', () => {
    const captured = [
      'event: response.created',
      'data: {"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}',
      '',
      'event: response.output_text.delta',
      'data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Codex 完整"}',
      '',
      'event: response.output_text.delta',
      'data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"结果"}',
      '',
      'event: response.completed',
      'data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Codex 完整结果"}]}]}}',
      '',
    ].join('\n')

    assert.deepEqual(
      formatCapturedContent(captured, 'text/event-stream', 'response'),
      {
        content: 'Codex 完整结果',
        renderMarkdown: true,
      }
    )
  })

  test('uses a completed Responses event when no text delta was captured', () => {
    const captured = [
      'event: response.created',
      'data: {"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}',
      '',
      'event: response.completed',
      'data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Buffered Codex result"}]}]}}',
      '',
    ].join('\n')

    assert.deepEqual(
      formatCapturedContent(captured, 'text/event-stream', 'response'),
      {
        content: 'Buffered Codex result',
        renderMarkdown: true,
      }
    )
  })

  test('keeps Responses SSE visible when the loaded segment has no output text', () => {
    const captured = [
      'event: response.created',
      'data: {"type":"response.created","response":{"id":"resp-1","status":"in_progress"}}',
      '',
      'event: response.output_item.added',
      'data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"item-1"}}',
      '',
    ].join('\n')

    assert.deepEqual(
      formatCapturedContent(captured, 'text/event-stream', 'response'),
      {
        content: captured,
        renderMarkdown: true,
      }
    )
  })

  test('shows message content from a non-streaming chat response', () => {
    const captured = JSON.stringify({
      id: 'completion-1',
      model: 'test-model',
      choices: [
        {
          index: 0,
          message: { role: 'assistant', content: 'Rendered response' },
          finish_reason: 'stop',
        },
      ],
      usage: { total_tokens: 12 },
    })

    assert.deepEqual(
      formatCapturedContent(captured, 'application/json', 'response'),
      {
        content: 'Rendered response',
        renderMarkdown: true,
      }
    )
  })

  test('shows only merged message content from a chat request as Markdown', () => {
    const captured = JSON.stringify({
      model: 'test-model',
      temperature: 0.5,
      messages: [
        { role: 'system', content: '## Instructions\n\nBe concise.' },
        { role: 'user', content: '**Check** the service.' },
      ],
    })

    assert.deepEqual(
      formatCapturedContent(captured, 'application/json', 'request'),
      {
        content: '## Instructions\n\nBe concise.\n\n**Check** the service.',
        renderMarkdown: true,
      }
    )
  })

  test('merges structured request content while ignoring non-text parts', () => {
    const captured = JSON.stringify({
      model: 'test-model',
      messages: [
        {
          role: 'user',
          content: [
            { type: 'text', text: 'Describe this image.' },
            {
              type: 'image_url',
              image_url: { url: 'data:image/png;base64,...' },
            },
            { type: 'text', text: '\nUse **Markdown**.' },
          ],
        },
      ],
    })

    assert.deepEqual(
      formatCapturedContent(captured, 'application/json', 'request'),
      {
        content: 'Describe this image.\nUse **Markdown**.',
        renderMarkdown: true,
      }
    )
  })

  test('keeps an error response visible when it has no assistant content', () => {
    const captured =
      '{"error":{"message":"upstream unavailable","type":"server_error"}}'

    assert.deepEqual(
      formatCapturedContent(captured, 'application/json', 'response'),
      {
        content: JSON.stringify(JSON.parse(captured), null, 2),
        renderMarkdown: false,
      }
    )
  })
})
