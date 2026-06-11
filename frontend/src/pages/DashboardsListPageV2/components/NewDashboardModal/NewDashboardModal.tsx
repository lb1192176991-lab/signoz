import { useEffect, useState } from 'react';
import { DialogWrapper } from '@signozhq/ui/dialog';
import { Tabs } from '@signozhq/ui/tabs';

import BlankDashboardPanel from './BlankDashboardPanel';
import ImportJsonPanel from './ImportJsonPanel';
import TemplatesPanel from './TemplatesPanel';

interface Props {
	open: boolean;
	onClose: () => void;
}

// Single entry point for creating a dashboard: blank form, a template gallery,
// or JSON import — as tabs in one modal (the modal opens in every case anyway,
// so this replaces the old split-button dropdown + three separate modals).
function NewDashboardModal({ open, onClose }: Props): JSX.Element {
	const [tab, setTab] = useState('blank');

	// Reset to the first tab whenever the modal reopens.
	useEffect(() => {
		if (open) {
			setTab('blank');
		}
	}, [open]);

	return (
		<DialogWrapper
			title="New dashboard"
			open={open}
			width="wide"
			onOpenChange={(next): void => {
				if (!next) {
					onClose();
				}
			}}
		>
			<Tabs
				value={tab}
				onChange={(key): void => setTab(key)}
				items={[
					{
						key: 'blank',
						label: 'Blank',
						children: <BlankDashboardPanel onClose={onClose} />,
					},
					{
						key: 'template',
						label: 'From a template',
						children: <TemplatesPanel />,
					},
					{
						key: 'import',
						label: 'Import JSON',
						children: <ImportJsonPanel />,
					},
				]}
			/>
		</DialogWrapper>
	);
}

export default NewDashboardModal;
