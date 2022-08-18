// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

import { TransactionInsightsViewProps } from "./transactionInsightsView";
import moment from "moment";
import { InsightExecEnum } from "../../types";

export const transactionInsightsPropsFixture: TransactionInsightsViewProps = {
  transactions: [
    {
      executionID: "f72f37ea-b3a0-451f-80b8-dfb27d0bc2a5",
      queries: [
        "SELECT IFNULL(a, b) FROM (SELECT (SELECT code FROM promo_codes WHERE code > $1 ORDER BY code LIMIT _) AS a, (SELECT code FROM promo_codes ORDER BY code LIMIT _) AS b)",
      ],
      insightName: "highWaitTime",
      startTime: moment.utc("2022.08.10"),
      elapsedTime: moment.duration("00:00:00.25").asMilliseconds(),
      application: "demo",
      execType: InsightExecEnum.TRANSACTION,
    },
    {
      executionID: "e72f37ea-b3a0-451f-80b8-dfb27d0bc2a5",
      queries: [
        "INSERT INTO vehicles VALUES ($1, $2, __more6__)",
        "INSERT INTO vehicles VALUES ($1, $2, __more6__)",
      ],
      insightName: "highWaitTime",
      startTime: moment.utc("2022.08.10"),
      elapsedTime: moment.duration("00:00:00.25").asMilliseconds(),
      application: "demo",
      execType: InsightExecEnum.TRANSACTION,
    },
    {
      executionID: "f72f37ea-b3a0-451f-80b8-dfb27d0bc2a0",
      queries: [
        "UPSERT INTO vehicle_location_histories VALUES ($1, $2, now(), $3, $4)",
      ],
      insightName: "highWaitTime",
      startTime: moment.utc("2022.08.10"),
      elapsedTime: moment.duration("00:00:00.25").asMilliseconds(),
      application: "demo",
      execType: InsightExecEnum.TRANSACTION,
    },
  ],
  transactionsError: null,
  sortSetting: {
    ascending: false,
    columnTitle: "startTime",
  },
  filters: {
    app: "",
  },
  refreshTransactionInsights: () => {},
  onSortChange: () => {},
  onFiltersChange: () => {},
};
