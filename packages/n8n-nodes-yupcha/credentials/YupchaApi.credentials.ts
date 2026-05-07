import {
	IAuthenticateGeneric,
	ICredentialType,
	INodeProperties,
} from 'n8n-workflow';

export class YupchaApi implements ICredentialType {
	name = 'yupchaApi';
	displayName = 'Yupcha API';
	documentationUrl = 'https://github.com/yupcha/n8n-nodes-yupcha';

	properties: INodeProperties[] = [
		{
			displayName: 'Instance URL',
			name: 'instanceUrl',
			type: 'string',
			default: 'http://localhost:8000',
			placeholder: 'https://your-yupcha-instance.com',
			description: 'The URL of your self-hosted Yupcha instance',
		},
		{
			displayName: 'API Key',
			name: 'apiKey',
			type: 'string',
			typeOptions: { password: true },
			default: '',
			description: 'Your Yupcha API key (Settings → API Keys)',
		},
	];

	authenticate: IAuthenticateGeneric = {
		type: 'generic',
		properties: {
			headers: {
				Authorization: '={{"Bearer " + $credentials.apiKey}}',
			},
		},
	};
}
