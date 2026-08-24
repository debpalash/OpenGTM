import {
	IAuthenticateGeneric,
	ICredentialType,
	INodeProperties,
} from 'n8n-workflow';

export class YupchaApi implements ICredentialType {
	name = 'yupchaApi';
	displayName = 'OpenGTM API';
	documentationUrl = 'https://github.com/debpalash/lead-data';

	properties: INodeProperties[] = [
		{
			displayName: 'Instance URL',
			name: 'instanceUrl',
			type: 'string',
			default: 'http://localhost:8000',
			placeholder: 'https://your-opengtm-instance.com',
			description: 'The URL of your self-hosted OpenGTM instance',
		},
		{
			displayName: 'API Key',
			name: 'apiKey',
			type: 'string',
			typeOptions: { password: true },
			default: '',
			description: 'Your OpenGTM API key (Settings → API Keys)',
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
