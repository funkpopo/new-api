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
import { useInfiniteQuery } from '@tanstack/react-query'
import { Check, Copy, LoaderCircle } from 'lucide-react'
import { useMemo, useState, type ReactNode, type UIEvent } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'

import { getLogContent } from '../../api'
import {
  decodeCapturedContent,
  isTextContentType,
} from '../../lib/captured-content'
import type { LogContentKind } from '../../types'

const displayPageSize = 8
const copyPageSize = 100

type CapturedContentSectionProps = {
  label: string
  requestId: string
  kind: LogContentKind
  enabled: boolean
}

export function CapturedContentSection(props: CapturedContentSectionProps) {
  const { t } = useTranslation()
  const { copiedText, copyToClipboard } = useCopyToClipboard({ notify: true })
  const [copying, setCopying] = useState(false)
  const query = useInfiniteQuery({
    queryKey: ['usage-log-content', props.requestId, props.kind],
    queryFn: ({ pageParam }) =>
      getLogContent(props.requestId, props.kind, pageParam, displayPageSize),
    initialPageParam: 1,
    getNextPageParam: (lastPage) => {
      const data = lastPage.data
      return data.page * data.page_size < data.total_chunks
        ? data.page + 1
        : undefined
    },
    enabled: props.enabled,
  })

  const pages = useMemo(
    () => query.data?.pages.map((page) => page.data) ?? [],
    [query.data]
  )
  const chunks = useMemo(() => pages.flatMap((page) => page.chunks), [pages])
  const contentType = pages[0]?.content_type ?? ''
  const displayedContent = useMemo(
    () => decodeCapturedContent(chunks, contentType),
    [chunks, contentType]
  )
  const totalChunks = pages[0]?.total_chunks ?? 0
  const totalSize = pages[0]?.total_size ?? 0
  const binary = pages.length > 0 && !isTextContentType(contentType)

  const handleScroll = (event: UIEvent<HTMLPreElement>) => {
    const element = event.currentTarget
    const closeToBottom =
      element.scrollHeight - element.scrollTop - element.clientHeight < 80
    if (closeToBottom && query.hasNextPage && !query.isFetchingNextPage) {
      void query.fetchNextPage()
    }
  }

  const handleCopy = async () => {
    if (pages.length === 0) return
    setCopying(true)
    try {
      const totalPages = Math.ceil(totalChunks / copyPageSize)
      const requests = Array.from({ length: totalPages }, (_, index) =>
        getLogContent(props.requestId, props.kind, index + 1, copyPageSize)
      )
      const responses = await Promise.all(requests)
      const allChunks = responses.flatMap((response) => response.data.chunks)
      await copyToClipboard(decodeCapturedContent(allChunks, contentType))
    } finally {
      setCopying(false)
    }
  }

  let copyIcon: ReactNode = <Copy className='size-3' aria-hidden='true' />
  if (copying) {
    copyIcon = (
      <LoaderCircle className='size-3 animate-spin' aria-hidden='true' />
    )
  } else if (copiedText !== null) {
    copyIcon = <Check className='size-3 text-green-600' aria-hidden='true' />
  }

  let content: ReactNode
  if (query.isPending) {
    content = (
      <div className='text-muted-foreground flex h-24 items-center justify-center gap-2 text-xs'>
        <LoaderCircle className='size-3.5 animate-spin' aria-hidden='true' />
        {t('Loading...')}
      </div>
    )
  } else if (query.isError) {
    content = (
      <p className='text-destructive py-6 text-center text-xs'>
        {t('Failed to load')}
      </p>
    )
  } else {
    content = (
      <>
        <pre
          className='bg-background/60 max-h-72 overflow-auto rounded border p-2 pr-8 font-mono text-[11px] leading-relaxed whitespace-pre-wrap'
          onScroll={handleScroll}
        >
          {displayedContent}
        </pre>
        <div className='text-muted-foreground mt-1 flex items-center justify-between gap-2 text-[11px]'>
          <span>
            {binary ? t('Binary content is displayed as Base64.') : null}
            {!binary && totalSize > 0
              ? `${totalSize.toLocaleString()} bytes`
              : null}
          </span>
          {query.hasNextPage && (
            <Button
              variant='ghost'
              size='sm'
              className='h-6 px-2 text-[11px]'
              onClick={() => void query.fetchNextPage()}
              disabled={query.isFetchingNextPage}
            >
              {query.isFetchingNextPage ? t('Loading...') : t('Load more')}
            </Button>
          )}
        </div>
      </>
    )
  }

  return (
    <section className='min-w-0 space-y-1.5'>
      <div className='flex items-center justify-between gap-2'>
        <Label className='flex items-center gap-1.5 text-xs font-semibold'>
          {props.label}
        </Label>
        {totalChunks > 0 && (
          <span className='text-muted-foreground text-[11px]'>
            {t('Loaded {{loaded}} of {{total}}', {
              loaded: chunks.length,
              total: totalChunks,
            })}
          </span>
        )}
      </div>
      <div className='bg-muted/30 min-w-0 overflow-hidden rounded-md border p-2.5 max-sm:p-2'>
        <div className='relative min-w-0'>
          <Button
            variant='ghost'
            size='sm'
            className='absolute top-0 right-0 z-10 h-6 w-6 p-0'
            onClick={() => void handleCopy()}
            disabled={copying || pages.length === 0}
            title={t('Copy complete content')}
            aria-label={t('Copy complete content')}
          >
            {copyIcon}
          </Button>
          {content}
        </div>
      </div>
    </section>
  )
}
