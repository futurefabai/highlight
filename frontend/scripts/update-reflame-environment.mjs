import * as jsoncParser from 'jsonc-parser'
import * as fs from 'node:fs'

const configPath = '../.reflame.config.jsonc'
const configText = fs.readFileSync(configPath, 'utf8')

const name = process.argv[2]

if (!name) {
	throw new Error('missing environment name')
}

const result = jsoncParser.applyEdits(
	configText,
	jsoncParser.modify(
		configText,
		['environments', name],
		{
			REACT_APP_FIREBASE_CONFIG_OBJECT:
				process.env.REACT_APP_FIREBASE_CONFIG_OBJECT?.replaceAll(
					'\n',
					'',
				) ??
				"{apiKey: 'AIzaSyD7g86A3EzEKmoE7aZ04Re3HZ0B4bWlL68',authDomain: 'auth.highlight.run',databaseURL: 'https://highlight-f5c5b.firebaseio.com',projectId: 'highlight-f5c5b',storageBucket: 'highlight-f5c5b.appspot.com',messagingSenderId: '263184175068',appId: '1:263184175068:web:f8190c20320087d1c6c919',}",
			REACT_APP_COMMIT_SHA: {
				type: 'expression',
				value: 'Reflame.gitCommitSha',
			},
			REACT_APP_PRIVATE_GRAPH_URI:
				process.env.REACT_APP_PRIVATE_GRAPH_URI ??
				'https://pri.highlight.run',
			REACT_APP_ONPREM: process.env.REACT_APP_ONPREM ?? false,
			REACT_APP_FRONTEND_URI: {
				type: 'expression',
				value: 'window.location.origin',
			},
			REACT_APP_FRONTEND_ORG: process.env.REACT_APP_FRONTEND_ORG ?? '1',
			REACT_APP_PUBLIC_GRAPH_URI:
				process.env.REACT_APP_PUBLIC_GRAPH_URI ??
				'https://pub.highlight.run',
			REACT_APP_AUTH_MODE: process.env.REACT_APP_AUTH_MODE ?? null,
			SLACK_CLIENT_ID: process.env.SLACK_CLIENT_ID ?? null,
			REACT_APP_STRIPE_API_PK:
				process.env.REACT_APP_STRIPE_API_PK ?? null,
			DEMO_ERROR_URL: process.env.DEMO_ERROR_URL ?? null,
			CLICKUP_CLIENT_ID: process.env.CLICKUP_CLIENT_ID ?? null,
			DISCORD_CLIENT_ID: process.env.DISCORD_CLIENT_ID ?? null,
			REACT_APP_FRONT_INTEGRATION_CLIENT_ID:
				process.env.REACT_APP_FRONT_INTEGRATION_CLIENT_ID ?? null,
			HEIGHT_CLIENT_ID: process.env.HEIGHT_CLIENT_ID ?? null,
			DEMO_SESSION_URL: process.env.DEMO_SESSION_URL ?? null,
			LINEAR_CLIENT_ID: process.env.LINEAR_CLIENT_ID ?? null,
			DEMO_PROJECT_ID: process.env.DEMO_PROJECT_ID ?? '2',
		},
		{ formattingOptions: { insertSpaces: true, tabSize: 2 } },
	),
)

const formatted = jsoncParser.applyEdits(
	result,
	jsoncParser.format(result, undefined, {
		insertSpaces: true,
		tabSize: 2,
	}),
)

if (formatted !== configText) {
	console.log(`updating Reflame config with '${name}' environment`)
	fs.writeFileSync(configPath, formatted)
}
