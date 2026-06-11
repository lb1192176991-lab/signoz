import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Button } from '@signozhq/ui/button';
import { Typography } from '@signozhq/ui/typography';
import { Plus } from '@signozhq/icons';
import logEvent from 'api/common/logEvent';
import { useListDashboardsV2 } from 'api/generated/services/dashboard';
import {
	DashboardtypesListOrderDTO,
	DashboardtypesListSortDTO,
} from 'api/generated/services/sigNoz.schemas';
import useComponentPermission from 'hooks/useComponentPermission';
import { useGetTenantLicense } from 'hooks/useGetTenantLicense';
import { useAppContext } from 'providers/App/App';
import { toAPIError } from 'utils/errorUtils';

import { combineQueries } from '../../filterQuery';
import { useActiveView } from '../../hooks/useActiveView';
import { useDashboardFilters } from '../../hooks/useDashboardFilters';
import {
	usePage,
	useSortColumn,
	useSortOrder,
} from '../../hooks/useDashboardsListQueryParams';
import { useDashboardViewsStore } from '../../store/useDashboardViewsStore';
import { useDashboardsListVisibleColumnsStore } from '../../store/useVisibleColumnsStore';
import type { UpdatedWindow } from '../../types';
import type { DashboardListItem } from '../../utils';
import { applyClientView, noResultsCopy } from '../../views';
import type { CreatorOption } from '../FilterZone/FilterChips';
import FilterZone from '../FilterZone/FilterZone';
import ListHeader from '../ListHeader/ListHeader';
import NewDashboardModal from '../NewDashboardModal/NewDashboardModal';
import StatusBar from '../StatusBar/StatusBar';
import EmptyState from '../states/EmptyState/EmptyState';
import ErrorState from '../states/ErrorState/ErrorState';
import LoadingState from '../states/LoadingState/LoadingState';
import NoResultsState from '../states/NoResultsState/NoResultsState';
import ViewsRail from '../ViewsRail/ViewsRail';
import DashboardsListContent from './DashboardsListContent';

import styles from './DashboardsList.module.scss';

const PAGE_SIZE = 20;
// Favorites / recently-viewed are filtered client-side (no server id filter), so
// we pull a single large page and constrain it in-memory.
const CLIENT_VIEW_LIMIT = 200;

function DashboardsList(): JSX.Element {
	const { isCloudUser } = useGetTenantLicense();

	const { user } = useAppContext();
	const [action, canCreateNewDashboard] = useComponentPermission(
		['action', 'create_new_dashboards'],
		user.role,
	);

	const {
		filters,
		query,
		isEmpty: filtersEmpty,
		setSearch,
		setCreatedBy,
		setUpdated,
		applyFilters,
		clearAll,
	} = useDashboardFilters();
	const [sortColumn, setSortColumn] = useSortColumn();
	const [sortOrder, setSortOrder] = useSortOrder();
	const [page, setPage] = usePage();

	const {
		activeViewId,
		builtinViews,
		customViews,
		isCustomActive,
		isModified,
		viewQuery,
		clientView,
		selectView,
		saveView,
		saveActiveView,
		resetView,
		removeView,
	} = useActiveView({ filters, applyFilters, userEmail: user.email });

	const railCollapsed = useDashboardViewsStore((s) => s.railCollapsed);
	const setRailCollapsed = useDashboardViewsStore((s) => s.setRailCollapsed);
	const favorites = useDashboardViewsStore((s) => s.favorites);
	const recent = useDashboardViewsStore((s) => s.recent);

	// Any filter change resets to the first page so the user isn't stranded on a
	// now-out-of-range offset.
	const handleSearchChange = useCallback(
		(value: string): void => {
			setSearch(value);
			void setPage(1);
		},
		[setSearch, setPage],
	);
	const handleCreatedByChange = useCallback(
		(emails: string[]): void => {
			setCreatedBy(emails);
			void setPage(1);
		},
		[setCreatedBy, setPage],
	);
	const handleUpdatedChange = useCallback(
		(window: UpdatedWindow): void => {
			setUpdated(window);
			void setPage(1);
		},
		[setUpdated, setPage],
	);
	const handleClearAll = useCallback((): void => {
		clearAll();
		void setPage(1);
	}, [clearAll, setPage]);

	// View actions that change the result set reset pagination too.
	const handleSelectView = useCallback(
		(id: string): void => {
			selectView(id);
			void setPage(1);
		},
		[selectView, setPage],
	);
	const handleResetView = useCallback((): void => {
		resetView();
		void setPage(1);
	}, [resetView, setPage]);
	const handleRemoveView = useCallback(
		(id: string): void => {
			removeView(id);
			void setPage(1);
		},
		[removeView, setPage],
	);
	const toggleRail = useCallback((): void => {
		setRailCollapsed(!railCollapsed);
	}, [setRailCollapsed, railCollapsed]);

	const listParams = useMemo(
		() => ({
			query: combineQueries(viewQuery, query) || undefined,
			sort: sortColumn,
			order: sortOrder,
			limit: clientView ? CLIENT_VIEW_LIMIT : PAGE_SIZE,
			offset: clientView ? 0 : (page - 1) * PAGE_SIZE,
		}),
		[viewQuery, query, sortColumn, sortOrder, page, clientView],
	);

	const {
		data: response,
		isLoading,
		isFetching,
		error,
		refetch,
	} = useListDashboardsV2(listParams, { query: { keepPreviousData: true } });

	const apiError = useMemo(
		() => (error ? toAPIError(error) : undefined),
		[error],
	);
	const errorHttpStatus = apiError?.getHttpStatusCode();
	const errorMessage = apiError?.getErrorMessage();

	const rawDashboards = useMemo<DashboardListItem[]>(
		() => response?.data?.dashboards ?? [],
		[response],
	);

	// Favorites / recently-viewed constrain the fetched rows by a client-side id
	// set; all other views are already constrained server-side.
	const dashboards = useMemo<DashboardListItem[]>(
		() =>
			clientView
				? applyClientView(rawDashboards, activeViewId, favorites, recent)
				: rawDashboards,
		[clientView, rawDashboards, activeViewId, favorites, recent],
	);
	const total = clientView ? dashboards.length : (response?.data?.total ?? 0);

	// Creator filter options: distinct authors on the loaded page plus the
	// current user (so "me" is always selectable). Page-scoped until a members
	// source backs this.
	const creatorOptions = useMemo<CreatorOption[]>(() => {
		const emails = new Set<string>();
		if (user.email) {
			emails.add(user.email);
		}
		rawDashboards.forEach((d) => {
			if (d.createdBy) {
				emails.add(d.createdBy);
			}
		});
		return [...emails].sort().map((email) => ({
			email,
			label: email === user.email ? `${email} (me)` : email,
		}));
	}, [rawDashboards, user.email]);

	const [isCreateOpen, setIsCreateOpen] = useState(false);
	const visibleColumns = useDashboardsListVisibleColumnsStore(
		(s) => s.visibleColumns,
	);

	const openCreate = useCallback((): void => {
		logEvent('Dashboard List: New dashboard clicked', {});
		setIsCreateOpen(true);
	}, []);

	const onSortChange = useCallback(
		(column: DashboardtypesListSortDTO): void => {
			void setSortColumn(column);
			void setPage(1);
		},
		[setSortColumn, setPage],
	);

	const onOrderChange = useCallback(
		(order: DashboardtypesListOrderDTO): void => {
			void setSortOrder(order);
			void setPage(1);
		},
		[setSortOrder, setPage],
	);

	const visitLoggedRef = useRef(false);
	useEffect(() => {
		if (!visitLoggedRef.current && !isLoading && response !== undefined) {
			logEvent('Dashboard List V2: Page visited', { number: dashboards.length });
			visitLoggedRef.current = true;
		}
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [isLoading]);

	// Heading reflects the active view (first-class views), e.g. "All dashboards".
	const activeLabel =
		customViews.find((v) => v.id === activeViewId)?.name ??
		builtinViews.find((v) => v.id === activeViewId)?.label ??
		'Dashboards';

	// The workspace-empty CTA ("create your first dashboard") belongs only to the
	// unfiltered All view; every other view's zero result is a no-results state.
	const showWorkspaceEmpty =
		!error &&
		dashboards.length === 0 &&
		activeViewId === 'all' &&
		filtersEmpty &&
		page === 1;

	const renderResults = (): JSX.Element => {
		if (isLoading) {
			return <LoadingState />;
		}
		if (error) {
			return (
				<ErrorState
					isCloudUser={!!isCloudUser}
					onRetry={(): void => {
						refetch();
					}}
					httpStatus={errorHttpStatus}
					errorMessage={errorMessage}
				/>
			);
		}
		if (dashboards.length === 0) {
			const copy = noResultsCopy(activeViewId, filters.search, !filtersEmpty);
			return <NoResultsState title={copy.title} description={copy.description} />;
		}
		return (
			<>
				<ListHeader
					sortColumn={sortColumn}
					onSortChange={onSortChange}
					sortOrder={sortOrder}
					onOrderChange={onOrderChange}
				/>
				<DashboardsListContent
					dashboards={dashboards}
					page={page}
					pageSize={clientView ? CLIENT_VIEW_LIMIT : PAGE_SIZE}
					total={total}
					onPageChange={setPage}
					canAct={!!action}
					showUpdatedAt={visibleColumns.updatedAt}
					showUpdatedBy={visibleColumns.updatedBy}
					loading={isFetching}
				/>
			</>
		);
	};

	const renderMain = (): JSX.Element => {
		// Pristine, unfiltered All view with nothing in the workspace → the
		// create-your-first-dashboard CTA, with no command zone to filter.
		if (showWorkspaceEmpty && !isLoading) {
			return (
				<div className={styles.emptyWrap}>
					<EmptyState
						createDropdown={
							canCreateNewDashboard ? (
								<Button
									variant="solid"
									color="primary"
									prefix={<Plus size={14} />}
									onClick={openCreate}
									testId="new-dashboard-cta"
								>
									New dashboard
								</Button>
							) : null
						}
					/>
				</div>
			);
		}
		return (
			<>
				<div className={styles.headerZone}>
					<div className={styles.commandHeader}>
						<div className={styles.headingBlock}>
							<Typography.Title className={styles.title}>
								{activeLabel}
							</Typography.Title>
							<span className={styles.countPill}>{total}</span>
						</div>
						<div className={styles.grow} />
						{canCreateNewDashboard && (
							<Button
								variant="solid"
								color="primary"
								prefix={<Plus size={14} />}
								onClick={openCreate}
								testId="new-dashboard-cta"
							>
								New dashboard
							</Button>
						)}
					</div>
					<FilterZone
						search={filters.search}
						createdBy={filters.createdBy}
						updated={filters.updated}
						creatorOptions={creatorOptions}
						isEmpty={filtersEmpty}
						onSearchChange={handleSearchChange}
						onCreatedByChange={handleCreatedByChange}
						onUpdatedChange={handleUpdatedChange}
						onClearAll={handleClearAll}
					/>
				</div>
				<div className={styles.viewContent}>{renderResults()}</div>
			</>
		);
	};

	return (
		<div className={styles.layout}>
			<ViewsRail
				activeViewId={activeViewId}
				builtinViews={builtinViews}
				customViews={customViews}
				isCustomActive={isCustomActive}
				isModified={isModified}
				collapsed={railCollapsed}
				onSelect={handleSelectView}
				onSave={saveView}
				onSaveChanges={saveActiveView}
				onReset={handleResetView}
				onClearFilters={handleClearAll}
				onDelete={handleRemoveView}
			/>
			<div className={styles.main}>
				<div className={styles.mainScroll}>{renderMain()}</div>
				<StatusBar
					collapsed={railCollapsed}
					onToggleCollapse={toggleRail}
					count={dashboards.length}
					total={total}
				/>
			</div>

			<NewDashboardModal
				open={isCreateOpen}
				onClose={(): void => setIsCreateOpen(false)}
			/>
		</div>
	);
}

export default DashboardsList;
