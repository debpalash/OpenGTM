import {
	IExecuteFunctions,
	INodeExecutionData,
	INodeType,
	INodeTypeDescription,
	NodeOperationError,
} from 'n8n-workflow';

export class Yupcha implements INodeType {
	description: INodeTypeDescription = {
		displayName: 'Yupcha',
		name: 'yupcha',
		icon: 'file:yupcha.svg',
		group: ['transform'],
		version: 1,
		subtitle: '={{$parameter["operation"]}}',
		description: 'Self-hosted GTM engine — source leads, enrich companies, verify emails',
		defaults: { name: 'Yupcha' },
		inputs: ['main'],
		outputs: ['main'],
		credentials: [
			{
				name: 'yupchaApi',
				required: true,
			},
		],
		properties: [
			// ── Operation ────────────────────────────────────────────
			{
				displayName: 'Operation',
				name: 'operation',
				type: 'options',
				noDataExpression: true,
				options: [
					{
						name: 'Source Leads',
						value: 'sourceLeads',
						description: 'Find leads across 95+ data sources',
						action: 'Source leads from 95 channels',
					},
					{
						name: 'Enrich Company',
						value: 'enrichCompany',
						description: 'Enrich a company with website, email, phone, social profiles',
						action: 'Enrich a company record',
					},
					{
						name: 'Verify Email',
						value: 'verifyEmail',
						description: 'SMTP-verify an email address for deliverability',
						action: 'Verify email deliverability',
					},
					{
						name: 'Score Lead',
						value: 'scoreLead',
						description: 'AI-score a lead for ICP fit (0-100)',
						action: 'Score a lead with AI',
					},
					{
						name: 'Get Tech Stack',
						value: 'getTechStack',
						description: 'Detect technologies used by a website (80+ fingerprints)',
						action: 'Detect tech stack from domain',
					},
					{
						name: 'Domain Intelligence',
						value: 'domainIntel',
						description: 'RDAP + DNS analysis — domain age, hosting, email provider',
						action: 'Analyze domain intelligence',
					},
					{
						name: 'Find Duplicates',
						value: 'findDuplicates',
						description: 'Fuzzy-match leads to find and merge duplicates',
						action: 'Find duplicate leads',
					},
				],
				default: 'sourceLeads',
			},

			// ── Source Leads Parameters ──────────────────────────────
			{
				displayName: 'Search Query',
				name: 'query',
				type: 'string',
				default: '',
				placeholder: 'IT staffing companies in Bangalore',
				description: 'Natural language query for lead sourcing',
				displayOptions: {
					show: { operation: ['sourceLeads'] },
				},
			},
			{
				displayName: 'Max Results',
				name: 'maxResults',
				type: 'number',
				default: 50,
				description: 'Maximum number of leads to return',
				displayOptions: {
					show: { operation: ['sourceLeads'] },
				},
			},

			// ── Enrich Parameters ────────────────────────────────────
			{
				displayName: 'Company Name',
				name: 'companyName',
				type: 'string',
				default: '',
				description: 'Company name to enrich',
				displayOptions: {
					show: { operation: ['enrichCompany'] },
				},
			},
			{
				displayName: 'Domain',
				name: 'domain',
				type: 'string',
				default: '',
				placeholder: 'example.com',
				description: 'Company domain/website to enrich',
				displayOptions: {
					show: { operation: ['enrichCompany', 'getTechStack', 'domainIntel'] },
				},
			},

			// ── Email Verify Parameters ──────────────────────────────
			{
				displayName: 'Email Address',
				name: 'email',
				type: 'string',
				default: '',
				placeholder: 'contact@company.com',
				description: 'Email address to verify',
				displayOptions: {
					show: { operation: ['verifyEmail'] },
				},
			},

			// ── Score Parameters ─────────────────────────────────────
			{
				displayName: 'Lead Data (JSON)',
				name: 'leadData',
				type: 'json',
				default: '{}',
				description: 'Lead record as JSON to score',
				displayOptions: {
					show: { operation: ['scoreLead'] },
				},
			},
		],
	};

	async execute(this: IExecuteFunctions): Promise<INodeExecutionData[][]> {
		const items = this.getInputData();
		const returnData: INodeExecutionData[] = [];
		const operation = this.getNodeParameter('operation', 0) as string;
		const credentials = await this.getCredentials('yupchaApi');
		const baseUrl = (credentials.instanceUrl as string).replace(/\/$/, '');

		for (let i = 0; i < items.length; i++) {
			try {
				let endpoint = '';
				let method = 'POST';
				let body: Record<string, unknown> = {};

				switch (operation) {
					case 'sourceLeads': {
						endpoint = '/api/jobs';
						body = {
							query: this.getNodeParameter('query', i) as string,
							max_results: this.getNodeParameter('maxResults', i) as number,
						};
						break;
					}
					case 'enrichCompany': {
						endpoint = '/api/leads/enrich';
						body = {
							company: this.getNodeParameter('companyName', i) as string,
							website: this.getNodeParameter('domain', i) as string,
						};
						break;
					}
					case 'verifyEmail': {
						endpoint = '/api/leads/verify-email';
						body = {
							email: this.getNodeParameter('email', i) as string,
						};
						break;
					}
					case 'scoreLead': {
						endpoint = '/api/leads/score';
						const leadDataStr = this.getNodeParameter('leadData', i) as string;
						body = JSON.parse(leadDataStr);
						break;
					}
					case 'getTechStack': {
						endpoint = '/api/leads/tech-stack';
						body = {
							domain: this.getNodeParameter('domain', i) as string,
						};
						break;
					}
					case 'domainIntel': {
						endpoint = '/api/leads/domain-intel';
						body = {
							domain: this.getNodeParameter('domain', i) as string,
						};
						break;
					}
					case 'findDuplicates': {
						endpoint = '/api/leads/dedup';
						method = 'POST';
						body = {};
						break;
					}
					default:
						throw new NodeOperationError(this.getNode(), `Unknown operation: ${operation}`);
				}

				const response = await this.helpers.httpRequest({
					method: method as 'GET' | 'POST',
					url: `${baseUrl}${endpoint}`,
					body,
					json: true,
				});

				returnData.push({ json: response });
			} catch (error) {
				if (this.continueOnFail()) {
					returnData.push({
						json: { error: (error as Error).message },
						pairedItem: { item: i },
					});
					continue;
				}
				throw error;
			}
		}

		return [returnData];
	}
}
