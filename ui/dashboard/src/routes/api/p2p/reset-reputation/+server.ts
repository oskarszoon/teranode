import { json } from '@sveltejs/kit'
import type { RequestHandler } from './$types'
import { dev } from '$app/environment'

/**
 * POST handler for /api/p2p/reset-reputation
 * Proxies requests to the Asset service's reset-reputation endpoint
 * (which in turn calls P2P's gRPC service)
 * Requires authentication - admin only
 */
export const POST: RequestHandler = async ({ request, url, cookies }) => {
  try {
    // Check authentication
    const sessionToken = cookies.get('session')

    if (!sessionToken) {
      return json(
        {
          error: 'Unauthorized',
          details: 'Authentication required',
        },
        { status: 401 },
      )
    }

    const body = await request.json()
    const { peer_id } = body

    let assetUrl: string

    if (dev) {
      // In development, Asset HTTP service runs on localhost:8090
      assetUrl = 'http://localhost:8090/api/p2p/reset-reputation'
    } else {
      // In production, construct URL based on current request
      const protocol = url.protocol === 'https:' ? 'https:' : 'http:'
      const host = url.hostname
      const port = process.env.ASSET_HTTP_PORT || '8090'
      assetUrl = `${protocol}//${host}:${port}/api/p2p/reset-reputation`
    }

    const response = await fetch(assetUrl, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({
        peer_id: peer_id || '',
      }),
    })

    if (!response.ok) {
      throw new Error(`Asset service returned ${response.status}: ${response.statusText}`)
    }

    const data = await response.json()
    return json(data)
  } catch (error) {
    console.error('Reset reputation proxy error:', error)
    return json(
      {
        error: 'Failed to reset reputation',
        details: error instanceof Error ? error.message : 'Unknown error',
      },
      { status: 500 },
    )
  }
}
